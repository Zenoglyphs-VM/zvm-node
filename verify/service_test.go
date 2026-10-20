package verify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	_ "modernc.org/sqlite"
)

// codeChain answers eth_getCode from a map the test can change, which is all
// the verification service reads from the chain.
type codeChain struct{ code map[common.Address][]byte }

func (c *codeChain) GetCode(_ context.Context, addr common.Address, _ string) (hexutil.Bytes, error) {
	return c.code[addr], nil
}

// cannedRunner stands in for solc: whatever the test says compiling produced.
type cannedRunner struct{ out string }

func (r *cannedRunner) Compile(context.Context, string, []byte) ([]byte, error) {
	return []byte(r.out), nil
}

func compiledAs(runtime string) string {
	return fmt.Sprintf(`{"contracts":{"C.sol":{"C":{"abi":[],"metadata":"{}","evm":{"bytecode":{"object":""},`+
		`"deployedBytecode":{"object":"%s","linkReferences":{},"immutableReferences":{}}}}}}}`, runtime)
}

const compileError = `{"errors":[{"severity":"error","type":"ParserError","message":"Expected pragma"}]}`

type verifyEnv struct {
	s      *Service
	db     *sql.DB
	chain  *codeChain
	runner *cannedRunner
	addr   common.Address
}

func newVerifyEnv(t *testing.T) *verifyEnv {
	t.Helper()
	chain := &codeChain{code: map[common.Address][]byte{}}
	srv := rpc.NewServer()
	if err := srv.RegisterName("eth", chain); err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(srv)
	t.Cleanup(node.Close)
	eth, err := ethclient.Dial(node.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eth.Close)

	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"builds":[{"path":"solc","version":"0.8.30","longVersion":"0.8.30+commit.73712a01","sha256":"00"}],"latestRelease":"0.8.30"}`)
	}))
	t.Cleanup(releases.Close)

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "v.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	runner := &cannedRunner{}
	s, err := New(Config{DB: db, Eth: eth, Compilers: &Compilers{BaseURL: releases.URL}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	addr := common.HexToAddress("0x00000000000000000000000000000000000c0de0")
	chain.code[addr] = mustHex(runtimeHex("0000", "11"))
	return &verifyEnv{s: s, db: db, chain: chain, runner: runner, addr: addr}
}

// run submits and compiles one job to the end, as a worker would.
func (e *verifyEnv) run(t *testing.T, input string) *Job {
	t.Helper()
	job, err := e.s.Submit(context.Background(), Request{
		Address: e.addr, Compiler: "0.8.30", Contract: "C.sol:C", Input: []byte(input),
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	e.s.process(context.Background(), job.ID)
	done, err := e.s.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

func (e *verifyEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func source(comment string) string {
	return `{"language":"Solidity","sources":{"C.sol":{"content":"// ` + comment + `\ncontract C {}"}},"settings":{}}`
}

// A finished job keeps no copy of what was submitted. Every input used to stay
// forever, failed ones too: up to 16 MB each, from a public endpoint, where a
// garbage source fails its compile in milliseconds.
func TestFinishedJobsKeepNoInput(t *testing.T) {
	e := newVerifyEnv(t)

	e.runner.out = compileError
	if job := e.run(t, source("does not compile")); job.Status != "failed" {
		t.Fatalf("a compile error: status %s", job.Status)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM verify_inputs"); n != 0 {
		t.Errorf("a failed job left %d inputs behind", n)
	}

	e.runner.out = compiledAs(runtimeHex("0000", "11"))
	if job := e.run(t, source("verifies")); job.Status != "verified" {
		t.Fatalf("a matching compile: status %s (%s)", job.Status, job.Message)
	}
	// One copy: the sanitized input the published contract was compiled from.
	if n := e.count(t, "SELECT COUNT(*) FROM verify_inputs"); n != 1 {
		t.Errorf("a verified contract should keep exactly its own input, found %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM verify_inputs i JOIN verified_contracts v ON v.input_hash = i.hash`); n != 1 {
		t.Error("the input kept is not the one the verified contract publishes")
	}
}

// A partial match cannot replace a partial one: it leaves comments, NatSpec
// and local names free, so it would let anyone rewrite what a verified
// contract's source says. An exact match may replace a partial one, and
// anything may replace a verification once the code itself has changed.
func TestPublishedSourcesAreReplacedOnlyByBetter(t *testing.T) {
	e := newVerifyEnv(t)
	published := func() string {
		var input string
		if err := e.db.QueryRow(`SELECT i.input FROM verified_contracts v JOIN verify_inputs i ON i.hash = v.input_hash
			WHERE v.address = ?`, e.addr.Hex()).Scan(&input); err != nil {
			t.Fatal(err)
		}
		return input
	}

	e.runner.out = compiledAs(runtimeHex("0000", "22")) // metadata differs: partial
	if job := e.run(t, source("the author's comments")); job.Status != "verified" || job.Match != Partial {
		t.Fatalf("first partial: %s %s (%s)", job.Status, job.Match, job.Message)
	}
	job := e.run(t, source("somebody else's comments"))
	if job.Status != "failed" || !strings.Contains(job.Message, "already verified") {
		t.Errorf("a second partial replaced the first: %s %s", job.Status, job.Message)
	}
	if !strings.Contains(published(), "the author's comments") {
		t.Error("the published source was rewritten by a second partial match")
	}

	e.runner.out = compiledAs(runtimeHex("0000", "11")) // metadata too: exact
	if job := e.run(t, source("the exact files")); job.Status != "verified" || job.Match != Exact {
		t.Fatalf("exact over partial: %s %s (%s)", job.Status, job.Match, job.Message)
	}
	if !strings.Contains(published(), "the exact files") {
		t.Error("an exact match did not replace the partial one")
	}

	// New code at the address: the old verification describes nothing there.
	e.chain.code[e.addr] = mustHex("6002" + "0000" + "5b" + "a1" + "33" + "000000" + "0005")
	e.runner.out = compiledAs("6002" + "0000" + "5b" + "a1" + "44" + "000000" + "0005")
	if job := e.run(t, source("the new code")); job.Status != "verified" {
		t.Fatalf("new code: %s (%s)", job.Status, job.Message)
	}
	if !strings.Contains(published(), "the new code") {
		t.Error("changed code kept the old verification")
	}
}

func TestSubmissionsAreRateLimited(t *testing.T) {
	e := newVerifyEnv(t)
	e.s.cfg.PerMinute = 2
	e.runner.out = compileError
	for i := 0; i < 2; i++ {
		e.run(t, source(fmt.Sprint(i)))
	}
	_, err := e.s.Submit(context.Background(), Request{Address: e.addr, Compiler: "0.8.30", Contract: "C.sol:C", Input: []byte(source("3"))})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("third submission in a minute: %v, want ErrRateLimited", err)
	}
	if !e.s.allow(time.Now().Add(time.Minute)) {
		t.Error("the limit should reset after a minute")
	}
}

func TestOldJobsArePruned(t *testing.T) {
	e := newVerifyEnv(t)
	e.runner.out = compileError
	e.run(t, source("old"))
	e.s.prune(context.Background(), time.Now().Add(keepFailedJobs+time.Minute))
	if n := e.count(t, "SELECT COUNT(*) FROM verify_jobs"); n != 0 {
		t.Errorf("a failed job past its time is still kept: %d rows", n)
	}
}

func TestOversizedNamesAreRefused(t *testing.T) {
	e := newVerifyEnv(t)
	_, err := e.s.Submit(context.Background(), Request{
		Address: e.addr, Compiler: "0.8.30", Contract: "C.sol:" + strings.Repeat("C", maxContractName), Input: []byte(source("x")),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a %d-byte contract name: %v, want ErrInvalid", maxContractName+6, err)
	}
}

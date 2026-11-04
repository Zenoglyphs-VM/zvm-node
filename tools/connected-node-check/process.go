package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"
	_ "modernc.org/sqlite"
)

type nodeProcess struct {
	cmd                  *exec.Cmd
	done                 chan struct{}
	err                  error
	log                  *boundedLog
	stopped              bool
	initialFrontierReads uint64
}

func startNode(ctx context.Context, dir string, nom *nomFixture) *nodeProcess {
	cmd := exec.CommandContext(ctx, "/qualified-node", "node", "-config", filepath.Join(dir, "rollup.json"), "-jwt", filepath.Join(dir, "jwt.hex"),
		"-db", filepath.Join(dir, "node.db"), "-l1", nom.url, "-engine", "http://127.0.0.1:8551", "-eth", "http://127.0.0.1:8545",
		"-verify=false", "-listen", "127.0.0.1:18080", "-mainnet-rpc", "off", "-prices", "off")
	n := &nodeProcess{cmd: cmd, done: make(chan struct{}), log: new(boundedLog), initialFrontierReads: nom.frontierReads.Load()}
	cmd.Stdout = n.log
	cmd.Stderr = n.log
	must(cmd.Start())
	go func() { n.err = cmd.Wait(); close(n.done) }()
	return n
}
func (n *nodeProcess) alive() {
	select {
	case <-n.done:
		fmt.Fprint(os.Stderr, n.log.text())
		panic(fmt.Sprintf("Node exited before requested shutdown: %v", n.err))
	default:
	}
}
func (n *nodeProcess) stop() {
	if n == nil || n.stopped {
		return
	}
	n.alive()
	n.stopped = true
	must(n.cmd.Process.Signal(os.Interrupt))
	select {
	case <-n.done:
	case <-time.After(10 * time.Second):
		must(n.cmd.Process.Kill())
		<-n.done
		fmt.Fprint(os.Stderr, n.log.text())
		panic("Node shutdown deadline")
	}
	var exit *exec.ExitError
	terminal := []string{}
	for _, line := range strings.Split(n.log.text(), "\n") {
		if strings.HasPrefix(line, "zvm-node node: ") {
			terminal = append(terminal, line)
		}
	}
	check(errors.As(n.err, &exit) && exit.ExitCode() == 1 && len(terminal) == 1 && strings.HasSuffix(terminal[0], ": context canceled"), "Node did not preserve its original cancellation exit")
}

// Only mode=ro observations are allowed. The observer never opens through
// store.Open, creates tables, writes rows, repairs a head or imports a payload.
func readOnlyDatabase(path string) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var readOnly int
	if err := db.QueryRow("PRAGMA query_only").Scan(&readOnly); err != nil {
		_ = db.Close()
		return nil, err
	}
	if readOnly != 1 {
		_ = db.Close()
		return nil, errors.New("SQL observer is not query-only")
	}
	return db, nil
}
func readNodeHead(ctx context.Context, path string) (uint64, uint64, string, string, uint64, uint64, uint64, uint64, error) {
	db, err := readOnlyDatabase(path)
	if err != nil {
		return 0, 0, "", "", 0, 0, 0, 0, err
	}
	defer db.Close()
	var height, number, txs, blocks, gas, attempts uint64
	var nomHash, blockHash string
	err = db.QueryRowContext(ctx, "SELECT height,l2_number,hash,l2_hash,txs,nom_blocks,gas_used,attempts FROM momentums ORDER BY height DESC LIMIT 1").Scan(&height, &number, &nomHash, &blockHash, &txs, &blocks, &gas, &attempts)
	return height, number, nomHash, blockHash, txs, blocks, gas, attempts, err
}
func waitNode(ctx context.Context, n *nodeProcess, nom *nomFixture, dir string, p *derive.Plan, number uint64) {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		n.alive()
		height, got, nomHash, _, txs, blocks, gas, attempts, err := readNodeHead(ctx, filepath.Join(dir, "node.db"))
		if err == nil {
			check(got <= number, "Node advanced beyond declared frontier")
			if got == number {
				check(height == p.Momentum.Height && nomHash == p.Momentum.Hash.String() && txs == 2 && blocks == 1 && gas > 0 && attempts == 1, "persisted Node derivation record differs")
				return
			}
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	fmt.Fprint(os.Stderr, n.log.text())
	panic("Node did not persist the declared frontier")
}
func waitNodeReady(ctx context.Context, n *nodeProcess, nom *nomFixture) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n.alive()
		if nom.frontierReads.Load() > n.initialFrontierReads {
			return
		}
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	fmt.Fprint(os.Stderr, n.log.text())
	panic("Node did not enter its derivation loop")
}
func writeNodeConfig(dir string, cfg *rollup.Config) {
	raw, err := jsonBytes(cfg)
	must(err)
	must(os.WriteFile(filepath.Join(dir, "rollup.json"), raw, 0600))
}

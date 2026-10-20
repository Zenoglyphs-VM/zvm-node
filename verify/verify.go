// Package verify publishes contract sources the way Etherscan, Sourcify and
// Blockscout do: recompile what the author submits, as solc standard JSON
// input, and accept it only if the output is the code on chain.
//
// The compiler runs on input from anyone, so it is fenced: sources must be
// inline (solc never reads a file or URL for this node), it runs in an empty
// directory with an empty environment, the model checker is removed, and a
// failure reports solc's messages but never its source excerpts — an excerpt
// of a file solc was tricked into opening would publish it.
package verify

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	// ErrInvalid marks a request the submitter can fix.
	ErrInvalid         = errors.New("invalid verification request")
	ErrAlreadyVerified = errors.New("contract source code already verified")
	ErrQueueFull       = errors.New("the verification queue is full; try again shortly")
)

type Config struct {
	DB        *sql.DB
	Eth       *ethclient.Client
	Compilers *Compilers
	Log       *slog.Logger
	// MaxInputBytes bounds a standard JSON input. Default 16 MB.
	MaxInputBytes int
	// Workers is how many contracts compile at once. Default 2: solc is the
	// heaviest thing this node runs, and a burst of submissions used to queue
	// behind one compile.
	Workers int
	// Runner compiles. Default: solc in this process's own container. The
	// compose stack sets a RemoteRunner pointing at the sandboxed `zvm solcd`
	// service, so the node never executes a compiler itself (runner.go).
	Runner Runner
	// Timeout bounds one compilation. Default 3 minutes.
	Timeout time.Duration
	// PerMinute is how many submissions the node accepts in a minute, from
	// everyone together. Default 60. A submission is anyone's, so the only
	// bound on what they cost is one the node sets itself.
	PerMinute int
}

type Service struct {
	cfg    Config
	db     *sql.DB
	queue  chan string
	runner Runner

	rateMu    sync.Mutex
	rateStart time.Time
	rateCount int
}

// What a verification row may hold. A job is anyone's to submit, so every
// field that reaches the database has a size, and every row a time to go.
const (
	maxContractName = 512
	maxCompilerName = 128
	maxMessage      = 2000
	// A failed job is kept long enough for whoever submitted it to read why;
	// a finished one long enough for forge or hardhat to poll it.
	keepFailedJobs   = 24 * time.Hour
	keepVerifiedJobs = 7 * 24 * time.Hour
	pruneEvery       = time.Hour
)

var ErrRateLimited = errors.New("too many verification requests; try again in a minute")

const (
	maxQueued = 16
	schema    = `
CREATE TABLE IF NOT EXISTS verified_contracts (
	address          TEXT PRIMARY KEY,
	match            TEXT    NOT NULL,
	runtime_hash     TEXT    NOT NULL,
	name             TEXT    NOT NULL,
	compiler         TEXT    NOT NULL,
	input            TEXT    NOT NULL,
	abi              TEXT    NOT NULL,
	metadata         TEXT,
	constructor_args TEXT,
	creation_match   TEXT,
	verified_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS verified_runtime ON verified_contracts(runtime_hash);
CREATE TABLE IF NOT EXISTS verify_jobs (
	id           TEXT PRIMARY KEY,
	address      TEXT    NOT NULL,
	compiler     TEXT    NOT NULL,
	contract     TEXT    NOT NULL,
	input        TEXT    NOT NULL,
	args         TEXT,
	status       TEXT    NOT NULL,
	message      TEXT,
	match        TEXT,
	created_at   INTEGER NOT NULL,
	finished_at  INTEGER
);
CREATE INDEX IF NOT EXISTS verify_jobs_status ON verify_jobs(status, created_at);
-- The standard JSON input is the big thing here — a quarter of a megabyte for
-- an ordinary project — and a job and the contract it verifies hold the same
-- bytes. Keyed by its own hash, it is stored once however many times it is
-- submitted, and re-verifying a contract costs a row rather than a copy.
CREATE TABLE IF NOT EXISTS verify_inputs (
	hash  TEXT PRIMARY KEY,
	input TEXT NOT NULL
);
`
)

func New(cfg Config) (*Service, error) {
	if cfg.MaxInputBytes == 0 {
		cfg.MaxInputBytes = 16 << 20
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if _, err := cfg.DB.Exec(schema); err != nil {
		return nil, fmt.Errorf("verification tables: %w", err)
	}
	// A job that was running when the node stopped will not finish.
	cfg.DB.Exec(`UPDATE verify_jobs SET status = 'failed', message = 'the node restarted before this job ran; submit it again',
		finished_at = ? WHERE status IN ('queued', 'running')`, time.Now().Unix())
	for _, col := range []string{
		"ALTER TABLE verify_jobs ADD COLUMN input_hash TEXT",
		"ALTER TABLE verified_contracts ADD COLUMN input_hash TEXT",
	} {
		if _, err := cfg.DB.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.PerMinute <= 0 {
		cfg.PerMinute = 60
	}
	runner := cfg.Runner
	if runner == nil {
		runner = &LocalRunner{Compilers: cfg.Compilers, Timeout: cfg.Timeout}
	}
	s := &Service{cfg: cfg, db: cfg.DB, queue: make(chan string, maxQueued), runner: runner}
	// Whatever an earlier run left: every input a finished job still held, and
	// jobs past their time.
	s.prune(context.Background(), time.Now())
	return s, nil
}

// allow counts one submission against the node's minute.
func (s *Service) allow(now time.Time) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if now.Sub(s.rateStart) >= time.Minute {
		s.rateStart, s.rateCount = now, 0
	}
	if s.rateCount >= s.cfg.PerMinute {
		return false
	}
	s.rateCount++
	return true
}

// dropInput forgets a submitted input once nothing needs it: no verified
// contract publishes it and no unfinished job will compile it. Before this,
// every input anyone submitted stayed forever, failed ones included — up to
// 16 MB each, from a public endpoint, and a garbage source fails its compile
// in milliseconds.
func (s *Service) dropInput(ctx context.Context, hash string) {
	if hash == "" {
		return
	}
	s.db.ExecContext(ctx, `DELETE FROM verify_inputs WHERE hash = ?
		AND NOT EXISTS (SELECT 1 FROM verified_contracts WHERE input_hash = ?)
		AND NOT EXISTS (SELECT 1 FROM verify_jobs WHERE input_hash = ? AND status IN ('queued', 'running'))`,
		hash, hash, hash)
}

// prune deletes finished jobs past their time and every input nothing refers
// to any more.
func (s *Service) prune(ctx context.Context, now time.Time) {
	s.db.ExecContext(ctx, `DELETE FROM verify_jobs WHERE (status = 'failed' AND finished_at < ?)
		OR (status = 'verified' AND finished_at < ?)`,
		now.Add(-keepFailedJobs).Unix(), now.Add(-keepVerifiedJobs).Unix())
	// A job row from before inputs were shared holds its input inline; a
	// finished one needs it no longer.
	s.db.ExecContext(ctx, `UPDATE verify_jobs SET input = '' WHERE status IN ('failed', 'verified') AND input != ''`)
	s.db.ExecContext(ctx, `DELETE FROM verify_inputs
		WHERE hash NOT IN (SELECT input_hash FROM verified_contracts WHERE input_hash IS NOT NULL)
		AND hash NOT IN (SELECT input_hash FROM verify_jobs WHERE status IN ('queued', 'running') AND input_hash IS NOT NULL)`)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Service) MaxInputBytes() int { return s.cfg.MaxInputBytes }

// Compilers is the release list, for a form to offer.
func (s *Service) Compilers(ctx context.Context) (*BuildList, error) {
	return s.cfg.Compilers.List(ctx)
}

// Run compiles queued jobs until ctx ends, Workers of them at a time, and
// prunes finished jobs hourly.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(pruneEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.prune(ctx, now)
			}
		}
	}()
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.queue:
					s.process(ctx, id)
				}
			}
		}()
	}
	wg.Wait()
}

// putInput stores a standard JSON input under its own hash and returns the
// hash, so that a job and the contract it verifies share one copy.
func (s *Service) putInput(ctx context.Context, input string) (string, error) {
	sum := crypto.Keccak256Hash([]byte(input)).Hex()
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO verify_inputs (hash, input) VALUES (?, ?)", sum, input)
	return sum, err
}

// input reads back what putInput stored, falling back to a row written before
// inputs were shared.
func (s *Service) input(ctx context.Context, hash, legacy string) string {
	if hash == "" {
		return legacy
	}
	var out string
	if err := s.db.QueryRowContext(ctx, "SELECT input FROM verify_inputs WHERE hash = ?", hash).Scan(&out); err != nil {
		return legacy
	}
	return out
}

// Request is one attempt to verify a contract.
type Request struct {
	Address common.Address
	// Compiler is 0.8.30, 0.8.30+commit.73712a01 or v0.8.30+commit.73712a01.
	Compiler string
	// Contract is path/File.sol:Name, or Name alone when only one contract has it.
	Contract string
	// Input is solc standard JSON input.
	Input json.RawMessage
	// ConstructorArgs is recorded only when the deploying transaction cannot
	// be found to read them from.
	ConstructorArgs string
}

type Job struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	Contract   string `json:"contract"`
	Compiler   string `json:"compiler"`
	Status     string `json:"status"` // queued, running, verified, failed
	Message    string `json:"message,omitempty"`
	Match      Match  `json:"match,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

// Submit checks what can be checked without compiling and queues the rest.
func (s *Service) Submit(ctx context.Context, req Request) (*Job, error) {
	if len(req.Input) > s.cfg.MaxInputBytes {
		return nil, fmt.Errorf("%w: input is larger than %d bytes", ErrInvalid, s.cfg.MaxInputBytes)
	}
	if _, err := sanitize(req.Input); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Contract) == "" {
		return nil, fmt.Errorf("%w: name the contract, as path/File.sol:Name", ErrInvalid)
	}
	if len(req.Contract) > maxContractName || len(req.Compiler) > maxCompilerName {
		return nil, fmt.Errorf("%w: the contract or compiler name is too long", ErrInvalid)
	}
	if !s.allow(time.Now()) {
		return nil, ErrRateLimited
	}
	if _, err := s.cfg.Compilers.Resolve(ctx, req.Compiler); err != nil {
		return nil, err
	}
	if req.ConstructorArgs != "" {
		if _, err := hex.DecodeString(strings.TrimPrefix(req.ConstructorArgs, "0x")); err != nil {
			return nil, fmt.Errorf("%w: constructor arguments must be hex", ErrInvalid)
		}
	}
	code, err := s.cfg.Eth.CodeAt(ctx, req.Address, nil)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("%w: there is no contract at %s", ErrInvalid, req.Address.Hex())
	}
	var match string
	if s.db.QueryRowContext(ctx, "SELECT match FROM verified_contracts WHERE address = ? AND runtime_hash = ?",
		req.Address.Hex(), crypto.Keccak256Hash(code).Hex()).Scan(&match) == nil && Match(match) == Exact {
		return nil, ErrAlreadyVerified
	}
	var queued int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM verify_jobs WHERE status IN ('queued', 'running')").Scan(&queued)
	if queued >= maxQueued {
		return nil, ErrQueueFull
	}

	idBytes := make([]byte, 16)
	rand.Read(idBytes)
	job := &Job{
		ID: hex.EncodeToString(idBytes), Address: req.Address.Hex(), Contract: strings.TrimSpace(req.Contract),
		Compiler: strings.TrimSpace(req.Compiler), Status: "queued", CreatedAt: time.Now().Unix(),
	}
	inputHash, err := s.putInput(ctx, string(req.Input))
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO verify_jobs (id, address, compiler, contract, input, input_hash, args, status, created_at)
		VALUES (?,?,?,?,'',?,?,?,?)`, job.ID, job.Address, job.Compiler, job.Contract, inputHash, req.ConstructorArgs, job.Status, job.CreatedAt); err != nil {
		return nil, err
	}
	select {
	case s.queue <- job.ID:
	default:
		s.finish(job.ID, "failed", ErrQueueFull.Error(), "")
		s.dropInput(ctx, inputHash)
		return nil, ErrQueueFull
	}
	return job, nil
}

func (s *Service) Job(ctx context.Context, id string) (*Job, error) {
	j := &Job{}
	var message, match sql.NullString
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, address, contract, compiler, status, message, match, created_at, finished_at
		FROM verify_jobs WHERE id = ?`, id).Scan(&j.ID, &j.Address, &j.Contract, &j.Compiler, &j.Status, &message, &match, &j.CreatedAt, &finished)
	if err != nil {
		return nil, err
	}
	j.Message, j.Match, j.FinishedAt = message.String, Match(match.String), finished.Int64
	return j, nil
}

func (s *Service) finish(id, status, message string, match Match) {
	s.db.Exec("UPDATE verify_jobs SET status = ?, message = ?, match = ?, finished_at = ? WHERE id = ?",
		status, clip(message, maxMessage), string(match), time.Now().Unix(), id)
}

func (s *Service) process(ctx context.Context, id string) {
	var req Request
	var addr, args string
	var input, inputHash string
	err := s.db.QueryRowContext(ctx, "SELECT address, compiler, contract, COALESCE(input,''), COALESCE(input_hash,''), COALESCE(args,'') FROM verify_jobs WHERE id = ?", id).
		Scan(&addr, &req.Compiler, &req.Contract, &input, &inputHash, &args)
	if err != nil {
		s.cfg.Log.Error("reading verification job", "id", id, "err", err)
		return
	}
	req.Address, req.Input, req.ConstructorArgs = common.HexToAddress(addr), json.RawMessage(s.input(ctx, inputHash, input)), args
	s.db.Exec("UPDATE verify_jobs SET status = 'running' WHERE id = ?", id)
	started := time.Now()
	match, err := s.verify(ctx, req)
	// The job's own copy of the input is done with either way: a verified
	// contract keeps the sanitized input it was compiled from, under its own
	// hash, and a failed job keeps only its message.
	defer s.dropInput(context.WithoutCancel(ctx), inputHash)
	if err != nil {
		s.finish(id, "failed", err.Error(), "")
		s.cfg.Log.Info("verification failed", "address", addr, "contract", req.Contract, "took", time.Since(started), "err", err)
		return
	}
	s.finish(id, "verified", "", match)
	s.cfg.Log.Info("contract verified", "address", addr, "contract", req.Contract, "match", match, "took", time.Since(started))
}

// --- compiling and matching ---------------------------------------------

type sourceFile struct {
	Content *string  `json:"content"`
	URLs    []string `json:"urls,omitempty"`
}

type stdInput struct {
	Language string                     `json:"language"`
	Sources  map[string]sourceFile      `json:"sources"`
	Settings map[string]json.RawMessage `json:"settings"`
}

var importPath = regexp.MustCompile(`import\s+(?:[^;"']*?\s+from\s+)?["']([^"']+)["']`)

// withoutComments blanks out a Solidity source's comments, keeping string
// literals as they are, so the import scan above sees what solc parses: a
// comment is whitespace to solc, and `import/**/"/config/jwt.hex";` had no
// whitespace for the scan to find.
func withoutComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		switch c := src[i]; {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // the closing slash
			b.WriteByte(' ')
		case c == '"' || c == '\'':
			b.WriteByte(c)
			for i++; i < len(src) && src[i] != c && src[i] != '\n'; i++ {
				if src[i] == '\\' && i+1 < len(src) {
					b.WriteByte(src[i])
					i++
				}
				b.WriteByte(src[i])
			}
			if i < len(src) {
				b.WriteByte(src[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isAbsolute(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || (len(p) > 1 && p[1] == ':')
}

// outputs is everything matching reads, and nothing else.
const outputs = `{"*":{"*":["abi","metadata","evm.bytecode.object","evm.bytecode.linkReferences",` +
	`"evm.deployedBytecode.object","evm.deployedBytecode.linkReferences","evm.deployedBytecode.immutableReferences"]}}`

// sanitize returns the standard JSON input this node will compile.
func sanitize(raw []byte) (*stdInput, error) {
	var in stdInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%w: not solc standard JSON input: %v", ErrInvalid, err)
	}
	if in.Language == "" {
		in.Language = "Solidity"
	}
	if in.Language != "Solidity" {
		return nil, fmt.Errorf("%w: only Solidity is verified here, not %s", ErrInvalid, in.Language)
	}
	if len(in.Sources) == 0 {
		return nil, fmt.Errorf("%w: no sources", ErrInvalid)
	}
	for name, f := range in.Sources {
		if f.Content == nil {
			return nil, fmt.Errorf("%w: source %q has no content; include every file inline, imports too", ErrInvalid, name)
		}
		if isAbsolute(name) {
			return nil, fmt.Errorf("%w: source %q: use a relative name", ErrInvalid, name)
		}
		for _, m := range importPath.FindAllStringSubmatch(withoutComments(*f.Content), -1) {
			if isAbsolute(m[1]) {
				return nil, fmt.Errorf("%w: %s imports %q; absolute imports are not compiled here", ErrInvalid, name, m[1])
			}
		}
		f.URLs = nil
		in.Sources[name] = f
	}
	if in.Settings == nil {
		in.Settings = map[string]json.RawMessage{}
	}
	if raw, ok := in.Settings["remappings"]; ok {
		var remaps []string
		if err := json.Unmarshal(raw, &remaps); err != nil {
			return nil, fmt.Errorf("%w: settings.remappings: %v", ErrInvalid, err)
		}
		for _, r := range remaps {
			if _, target, ok := strings.Cut(r, "="); ok && isAbsolute(target) {
				return nil, fmt.Errorf("%w: remapping %q points at an absolute path", ErrInvalid, r)
			}
		}
	}
	delete(in.Settings, "modelChecker")
	delete(in.Settings, "stopAfter")
	in.Settings["outputSelection"] = json.RawMessage(outputs)
	return &in, nil
}

type compiled struct {
	ABI      json.RawMessage `json:"abi"`
	Metadata string          `json:"metadata"`
	EVM      struct {
		Bytecode struct {
			Object         string   `json:"object"`
			LinkReferences linkRefs `json:"linkReferences"`
		} `json:"bytecode"`
		DeployedBytecode struct {
			Object              string        `json:"object"`
			LinkReferences      linkRefs      `json:"linkReferences"`
			ImmutableReferences immutableRefs `json:"immutableReferences"`
		} `json:"deployedBytecode"`
	} `json:"evm"`
}

type solcOutput struct {
	Errors []struct {
		Severity string `json:"severity"`
		Type     string `json:"type"`
		Message  string `json:"message"`
	} `json:"errors"`
	Contracts map[string]map[string]compiled `json:"contracts"`
}

func (s *Service) compile(ctx context.Context, compiler string, in *stdInput) (*solcOutput, error) {
	build, err := s.cfg.Compilers.Resolve(ctx, compiler)
	if err != nil {
		return nil, err
	}
	input, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	raw, err := s.runner.Compile(ctx, compiler, input)
	if err != nil {
		return nil, err
	}
	var out solcOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("solc %s answered with something other than JSON", build.LongVersion)
	}
	var problems []string
	for _, e := range out.Errors {
		if e.Severity == "error" {
			problems = append(problems, strings.TrimSpace(e.Type+": "+e.Message))
		}
	}
	if len(problems) > 0 {
		if len(problems) > 5 {
			problems = append(problems[:5], fmt.Sprintf("and %d more", len(problems)-5))
		}
		return nil, fmt.Errorf("compilation failed with solc %s: %s", build.LongVersion, strings.Join(problems, "; "))
	}
	return &out, nil
}

// pick finds the contract a request names.
func pick(out *solcOutput, name string) (string, *compiled, error) {
	file, contract, hasPath := strings.Cut(name, ":")
	if !hasPath {
		contract, file = file, ""
	}
	if hasPath {
		if i := strings.LastIndex(name, ":"); i >= 0 {
			file, contract = name[:i], name[i+1:]
		}
		c, ok := out.Contracts[file][contract]
		if !ok {
			return "", nil, fmt.Errorf("%w: the compiler produced no contract %s", ErrInvalid, name)
		}
		return file + ":" + contract, &c, nil
	}
	var found []string
	var hit compiled
	for f, cs := range out.Contracts {
		if c, ok := cs[contract]; ok {
			found = append(found, f+":"+contract)
			hit = c
		}
	}
	switch len(found) {
	case 0:
		return "", nil, fmt.Errorf("%w: the compiler produced no contract named %s", ErrInvalid, contract)
	case 1:
		return found[0], &hit, nil
	}
	sort.Strings(found)
	return "", nil, fmt.Errorf("%w: more than one contract is named %s (%s); give its path", ErrInvalid, contract, strings.Join(found, ", "))
}

func (s *Service) verify(ctx context.Context, req Request) (Match, error) {
	in, err := sanitize(req.Input)
	if err != nil {
		return "", err
	}
	out, err := s.compile(ctx, req.Compiler, in)
	if err != nil {
		return "", err
	}
	name, c, err := pick(out, req.Contract)
	if err != nil {
		return "", err
	}
	if strings.TrimPrefix(c.EVM.DeployedBytecode.Object, "0x") == "" {
		return "", fmt.Errorf("%w: %s has no runtime code — an interface or abstract contract cannot be deployed", ErrInvalid, name)
	}
	code, err := s.cfg.Eth.CodeAt(ctx, req.Address, nil)
	if err != nil {
		return "", err
	}
	match, meta, err := compareRuntime(c.EVM.DeployedBytecode.Object, c.EVM.DeployedBytecode.LinkReferences, c.EVM.DeployedBytecode.ImmutableReferences, code)
	if err != nil {
		return "", err
	}
	if match == "" {
		return "", fmt.Errorf("the runtime bytecode compiled from %s (%d bytes) is not the code at %s (%d bytes); check the compiler version, optimizer runs, EVM version and via-IR setting",
			name, len(strings.TrimPrefix(c.EVM.DeployedBytecode.Object, "0x"))/2, req.Address.Hex(), len(code))
	}

	// The constructor's arguments, when the contract was created by a
	// transaction rather than by another contract.
	var creation Match
	args := strings.TrimPrefix(req.ConstructorArgs, "0x")
	var txHash string
	if s.db.QueryRowContext(ctx, "SELECT tx FROM contracts WHERE address = ?", req.Address.Hex()).Scan(&txHash) == nil {
		if tx, _, err := s.cfg.Eth.TransactionByHash(ctx, common.HexToHash(txHash)); err == nil && tx.To() == nil {
			m, found, err := constructorArgs(c.EVM.Bytecode.Object, c.EVM.Bytecode.LinkReferences, meta, tx.Data())
			if err == nil && m != "" {
				creation, args = m, hex.EncodeToString(found)
			}
		}
	}

	// Published sources are replaced only by something better: an exact match
	// over a partial one, or anything at all once the code at the address has
	// changed. A partial match leaves comments, NatSpec and local names free,
	// so letting one partial replace another let anyone rewrite what a
	// verified contract's source says it does. Sourcify's rule is the same.
	runtimeHash := crypto.Keccak256Hash(code).Hex()
	var had, hadHash string
	if s.db.QueryRowContext(ctx, "SELECT match, runtime_hash FROM verified_contracts WHERE address = ?",
		req.Address.Hex()).Scan(&had, &hadHash) == nil && hadHash == runtimeHash && !(Match(had) == Partial && match == Exact) {
		return "", fmt.Errorf("%w (%s match); only an exact match can replace a partial one", ErrAlreadyVerified, had)
	}

	stored, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	inputHash, err := s.putInput(ctx, string(stored))
	if err != nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO verified_contracts
		(address, match, runtime_hash, name, compiler, input, input_hash, abi, metadata, constructor_args, creation_match, verified_at)
		VALUES (?,?,?,?,?,'',?,?,?,?,?,?)
		ON CONFLICT(address) DO UPDATE SET match = excluded.match, runtime_hash = excluded.runtime_hash, name = excluded.name,
			compiler = excluded.compiler, input = excluded.input, input_hash = excluded.input_hash, abi = excluded.abi, metadata = excluded.metadata,
			constructor_args = excluded.constructor_args, creation_match = excluded.creation_match, verified_at = excluded.verified_at
		WHERE verified_contracts.runtime_hash != excluded.runtime_hash
			OR (verified_contracts.match != 'exact' AND excluded.match = 'exact')`,
		req.Address.Hex(), string(match), runtimeHash, name, resolvedVersion(ctx, s, req.Compiler), inputHash,
		string(c.ABI), c.Metadata, args, string(creation), time.Now().Unix())
	if err != nil {
		return "", err
	}
	// Another job got there first with an equal or better match.
	if n, _ := res.RowsAffected(); n == 0 {
		s.dropInput(ctx, inputHash)
		return "", fmt.Errorf("%w; only an exact match can replace a partial one", ErrAlreadyVerified)
	}
	return match, nil
}

func resolvedVersion(ctx context.Context, s *Service, v string) string {
	if b, err := s.cfg.Compilers.Resolve(ctx, v); err == nil {
		return b.LongVersion
	}
	return v
}

// --- reading --------------------------------------------------------------

// Verified is a contract's published source.
type Verified struct {
	Address         string            `json:"address"`
	Match           Match             `json:"match"`
	Name            string            `json:"name"`
	Compiler        string            `json:"compiler"`
	ABI             json.RawMessage   `json:"abi"`
	Sources         map[string]string `json:"sources"`
	Settings        json.RawMessage   `json:"settings"`
	ConstructorArgs string            `json:"constructorArgs,omitempty"`
	CreationMatch   Match             `json:"creationMatch,omitempty"`
	VerifiedAt      int64             `json:"verifiedAt"`
	// SimilarTo is the address these sources were verified at, when this
	// contract's runtime code is byte for byte that one's.
	SimilarTo string `json:"similarTo,omitempty"`
	// Input is the standard JSON input that was compiled.
	Input json.RawMessage `json:"-"`
}

// Lookup is the verified source of the contract whose runtime code is code:
// its own, or failing that another address's with identical code — what
// Etherscan calls a similar match.
func (s *Service) Lookup(ctx context.Context, addr common.Address, code []byte) (*Verified, error) {
	if len(code) == 0 {
		return nil, nil
	}
	hash := crypto.Keccak256Hash(code).Hex()
	row := s.db.QueryRowContext(ctx, `SELECT address, match, name, compiler, COALESCE(input,''), COALESCE(input_hash,''), abi, COALESCE(constructor_args,''),
		COALESCE(creation_match,''), verified_at FROM verified_contracts
		WHERE runtime_hash = ? ORDER BY address = ? DESC, match = 'exact' DESC, verified_at LIMIT 1`, hash, addr.Hex())
	v := &Verified{}
	var match, input, inputHash, abi, creation string
	err := row.Scan(&v.Address, &match, &v.Name, &v.Compiler, &input, &inputHash, &abi, &v.ConstructorArgs, &creation, &v.VerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.Match, v.CreationMatch, v.ABI = Match(match), Match(creation), json.RawMessage(abi)
	v.Input = json.RawMessage(s.input(ctx, inputHash, input))
	if v.Address != addr.Hex() {
		// Another deployment's constructor arguments are not this one's.
		v.SimilarTo, v.ConstructorArgs, v.CreationMatch = v.Address, "", ""
		v.Address = addr.Hex()
	}
	var in stdInput
	if json.Unmarshal(v.Input, &in) == nil {
		v.Sources = map[string]string{}
		for name, f := range in.Sources {
			if f.Content != nil {
				v.Sources[name] = *f.Content
			}
		}
		settings := map[string]json.RawMessage{}
		for k, val := range in.Settings {
			if k != "outputSelection" {
				settings[k] = val
			}
		}
		v.Settings, _ = json.Marshal(settings)
	}
	return v, nil
}

// --- proxies ----------------------------------------------------------------

type Proxy struct {
	Kind           string `json:"kind"` // eip1167, eip1967, eip1967-beacon
	Implementation string `json:"implementation"`
}

var (
	minimalPrefix = common.FromHex("363d3d373d3d3d363d73")
	minimalSuffix = common.FromHex("5af43d82803e903d91602b57fd5bf3")
	// keccak256("eip1967.proxy.implementation") - 1, and the beacon slot.
	implementationSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
	beaconSlot         = common.HexToHash("0xa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50")
)

// DetectProxy recognises an EIP-1167 minimal proxy, which the ZVM's NFT
// collections are, and EIP-1967 proxies, and names the code they run.
func DetectProxy(ctx context.Context, eth *ethclient.Client, addr common.Address, code []byte) *Proxy {
	if len(code) == 45 && bytes.HasPrefix(code, minimalPrefix) && bytes.HasSuffix(code, minimalSuffix) {
		return &Proxy{Kind: "eip1167", Implementation: common.BytesToAddress(code[10:30]).Hex()}
	}
	if raw, err := eth.StorageAt(ctx, addr, implementationSlot, nil); err == nil {
		if impl := common.BytesToAddress(raw); impl != (common.Address{}) {
			return &Proxy{Kind: "eip1967", Implementation: impl.Hex()}
		}
	}
	if raw, err := eth.StorageAt(ctx, addr, beaconSlot, nil); err == nil {
		if beacon := common.BytesToAddress(raw); beacon != (common.Address{}) {
			// implementation()
			msg := callMsg(beacon, common.FromHex("0x5c60da1b"))
			if out, err := eth.CallContract(ctx, msg, nil); err == nil && len(out) == 32 {
				return &Proxy{Kind: "eip1967-beacon", Implementation: common.BytesToAddress(out).Hex()}
			}
		}
	}
	return nil
}

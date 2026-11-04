package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/l2"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

type boundedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len()+len(p) > 512<<10 {
		return 0, errors.New("client diagnostic cap exceeded")
	}
	return b.b.Write(p)
}
func (b *boundedLog) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type client struct {
	cmd     *exec.Cmd
	done    chan error
	log     *boundedLog
	stopped bool
}

func startClient(ctx context.Context, dir string) *client {
	genesis := filepath.Join(dir, "genesis.json")
	data := filepath.Join(dir, "db")
	if _, err := os.Stat(data); os.IsNotExist(err) {
		c := exec.CommandContext(ctx, "/usr/local/bin/op-reth", "init", "--chain", genesis, "--datadir", data)
		b := new(boundedLog)
		c.Stdout = b
		c.Stderr = b
		err := c.Run()
		if err != nil {
			fmt.Fprint(os.Stderr, b.text())
			must(err)
		}
	}
	cmd := exec.CommandContext(ctx, "/usr/local/bin/op-reth", "node", "--chain", genesis, "--datadir", data,
		"--http", "--http.addr", "127.0.0.1", "--http.port", "8545", "--http.api", "eth,net,web3",
		"--authrpc.addr", "127.0.0.1", "--authrpc.port", "8551", "--authrpc.jwtsecret", filepath.Join(dir, "jwt.hex"),
		"--ipcdisable", "--disable-discovery", "--max-outbound-peers", "0", "--max-inbound-peers", "0", "--port", "0", "--rollup.disable-tx-pool-gossip")
	c := &client{cmd: cmd, done: make(chan error, 1), log: new(boundedLog)}
	cmd.Stdout = c.log
	cmd.Stderr = c.log
	must(cmd.Start())
	go func() { c.done <- cmd.Wait() }()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-c.done:
			fmt.Fprint(os.Stderr, c.log.text())
			panic(fmt.Sprintf("client terminated before readiness: %v", err))
		default:
		}
		request, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8545", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
		must(err)
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: time.Second}).Do(request)
		if err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			must(response.Body.Close())
			if readErr == nil && response.StatusCode == 200 && len(raw) <= 4096 && bytes.Contains(raw, []byte(`"result"`)) {
				return c
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprint(os.Stderr, c.log.text())
	c.stop()
	panic("client RPC readiness deadline")
}
func (c *client) stop() {
	if c == nil || c.stopped {
		return
	}
	c.stopped = true
	must(c.cmd.Process.Signal(syscall.SIGINT))
	select {
	case err := <-c.done:
		if err != nil {
			fmt.Fprint(os.Stderr, c.log.text())
			must(err)
		}
	case <-time.After(15 * time.Second):
		must(c.cmd.Process.Kill())
		<-c.done
		fmt.Fprint(os.Stderr, c.log.text())
		panic("client shutdown deadline")
	}
}
func l2Predeploys() map[string]common.Address { return l2.Predeploys() }
func unauthenticatedRefused(ctx context.Context) {
	request, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:8551", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"engine_exchangeCapabilities","params":[[]]}`))
	must(err)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	must(err)
	must(response.Body.Close())
	check(response.StatusCode == 401, "unauthenticated Engine request admitted")
}
func checkGenesis(ctx context.Context, c *rpc.Client, g *core.Genesis) {
	var header *types.Header
	must(c.CallContext(ctx, &header, "eth_getBlockByNumber", "0x0", false))
	check(header != nil && header.Hash() == g.ToBlock().Hash() && header.Root == g.ToBlock().Root(), "full client genesis header differs")
	var peers hexutil.Uint64
	must(c.CallContext(ctx, &peers, "net_peerCount"))
	check(peers == 0, "client has peers")
	for a, account := range g.Alloc {
		var code hexutil.Bytes
		must(c.CallContext(ctx, &code, "eth_getCode", a, "0x0"))
		check(bytes.Equal(code, account.Code), "installed genesis code differs")
	}
}
func checkAccounts(ctx context.Context, c *rpc.Client, st *state.StateDB) {
	for _, a := range addresses() {
		var proof struct {
			Address     common.Address `json:"address"`
			Balance     *hexutil.Big   `json:"balance"`
			Nonce       hexutil.Uint64 `json:"nonce"`
			CodeHash    common.Hash    `json:"codeHash"`
			StorageHash common.Hash    `json:"storageHash"`
		}
		must(c.CallContext(ctx, &proof, "eth_getProof", a, []common.Hash{}, "latest"))
		check(proof.Address == a && proof.Balance != nil && proof.Balance.ToInt().Cmp(st.GetBalance(a).ToBig()) == 0 && uint64(proof.Nonce) == st.GetNonce(a), "account nonce or balance differs")
		check(proof.CodeHash == crypto.Keccak256Hash(st.GetCode(a)), "account code hash differs")
		check(proof.StorageHash == st.GetStorageRoot(a), "account storage root differs")
	}
}
func checkStored(ctx context.Context, c *rpc.Client, payload *engine.ExecutableData, p *derive.Plan, st *state.StateDB) {
	var header *types.Header
	must(c.CallContext(ctx, &header, "eth_getBlockByNumber", "latest", false))
	check(header != nil && header.Hash() == payload.BlockHash && header.ParentHash == payload.ParentHash && header.Root == payload.StateRoot && header.Number.Uint64() == payload.Number, "stored complete header differs")
	for i, planned := range p.Txs {
		var receipt *types.Receipt
		must(c.CallContext(ctx, &receipt, "eth_getTransactionReceipt", planned.Tx.Hash()))
		check(receipt != nil && receipt.Status == 1 && receipt.TxHash == planned.Tx.Hash() && receipt.BlockHash == payload.BlockHash && receipt.TransactionIndex == uint(i) && receipt.BlockNumber.Uint64() == payload.Number && receipt.GasUsed > 0, "stored receipt differs")
		check(receipt.DepositNonce != nil && *receipt.DepositNonce == payload.Number-1, "deposit receipt nonce differs")
	}
	checkAccounts(ctx, c, st)
}

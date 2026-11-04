package indexer

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

func uriResponse(t *testing.T, text string) []byte {
	t.Helper()
	typ, err := abi.NewType("string", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (abi.Arguments{{Type: typ}}).Pack(text)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestObservedURIDecoderRefusesIncompleteEvidence(t *testing.T) {
	for _, name := range []string{"empty", "string", "bytes32", "short", "length-overflow", "offset-overflow", "oversized", "invalid-utf8"} {
		t.Run(name, func(t *testing.T) {
			value, want, valid := uriResponse(t, "ipfs://document"), "ipfs://document", true
			switch name {
			case "empty":
				value, want = uriResponse(t, ""), ""
			case "bytes32":
				value = make([]byte, 32)
				copy(value, []byte(want))
			case "short":
				value, valid = []byte{1}, false
			case "length-overflow":
				value[32] = 255
				valid = false
			case "offset-overflow":
				value[0] = 255
				valid = false
			case "oversized":
				value = bytes.Repeat([]byte{1}, MaxURIBytes+97)
				valid = false
			case "invalid-utf8":
				value = uriResponse(t, string([]byte{255}))
				valid = false
			}
			got, err := decodeObservedURI(value)
			if valid && (err != nil || got != want) {
				t.Fatalf("decoded %q, %v; want %q", got, err, want)
			}
			if !valid && err == nil {
				t.Fatal("incomplete or invalid URI became a proven empty value")
			}
		})
	}
}

type uriHashRPC struct {
	hash   common.Hash
	answer []byte
	calls  atomic.Int32
}

func (r *uriHashRPC) Call(_ context.Context, args callArgs, block rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if block.BlockHash == nil || *block.BlockHash != r.hash || block.BlockNumber != nil {
		return nil, fmtURIError{}
	}
	if args.Gas == nil || uint64(*args.Gas) != uriGas {
		return nil, fmtURIError{}
	}
	r.calls.Add(1)
	return r.answer, nil
}

type fmtURIError struct{}

func (fmtURIError) Error() string { return "URI call did not select the required hash and gas" }

func TestURIHostUsesTheExactHashWithoutLatestFallback(t *testing.T) {
	backend := &uriHashRPC{hash: common.Hash{31: 71}, answer: uriResponse(t, "data:application/json,{}")}
	server := rpc.NewServer()
	if err := server.RegisterName("eth", backend); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	client := rpc.DialInProc(server)
	t.Cleanup(client.Close)
	ix := &Indexer{eth: ethclient.NewClient(client)}
	uri, err := ix.CallURIAtHash(context.Background(), common.Address{19: 3}, []byte{1, 2, 3, 4}, backend.hash)
	if err != nil || uri != "data:application/json,{}" || backend.calls.Load() != 1 {
		t.Fatalf("hash URI call: %q %v %d", uri, err, backend.calls.Load())
	}
	if _, err := ix.CallURIAtHash(context.Background(), common.Address{}, nil, common.Hash{}); err == nil || backend.calls.Load() != 1 {
		t.Fatal("missing source hash reached the RPC or became a verified URI")
	}
}

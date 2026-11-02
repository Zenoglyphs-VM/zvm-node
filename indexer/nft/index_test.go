package nft

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// slowHost answers every URI call, slowly: a collection whose tokenURI costs
// something to run.
type slowHost struct {
	delay time.Duration
	calls int
}

func (h *slowHost) Eth() *ethclient.Client { return nil }
func (h *slowHost) Log() *slog.Logger      { return slog.New(slog.DiscardHandler) }
func (h *slowHost) CallString(context.Context, common.Address, string) string {
	return ""
}
func (h *slowHost) CallURI(context.Context, common.Address, []byte) string {
	return "unexpected-latest"
}
func (h *slowHost) CallURIAtHash(_ context.Context, _ common.Address, _ []byte, hash common.Hash) (string, error) {
	if hash == (common.Hash{}) {
		return "", fmt.Errorf("missing observed hash")
	}
	time.Sleep(h.delay)
	h.calls++
	return "ipfs://token", nil
}

// A Transfer is cheap to emit and each first sighting of a token costs a
// tokenURI call, so one block's metadata reads share a budget. Past it tokens
// are indexed without metadata rather than holding the chain's index back;
// the next block starts afresh.
func TestMetadataReadsStopAtTheBlocksBudget(t *testing.T) {
	defer func(old time.Duration) { metadataBudget = old }(metadataBudget)
	metadataBudget = 60 * time.Millisecond
	h := &slowHost{delay: 25 * time.Millisecond}
	x := New(Config{})
	if err := x.Attach(h); err != nil {
		t.Fatal(err)
	}
	collection := common.HexToAddress("0xc011")
	ctx := context.Background()
	read := 0
	for i := 0; i < 20; i++ {
		if x.tokenURI(withMetadataBlock(ctx, 7, common.Hash{31: 7}), 7, collection, common.Big1) != "" {
			read++
		}
	}
	if read == 0 || read >= 20 || h.calls != read {
		t.Errorf("block 7 read %d of 20 URIs with %d calls; want the budget to stop it after a few", read, h.calls)
	}
	if x.tokenURI(withMetadataBlock(ctx, 8, common.Hash{31: 8}), 8, collection, common.Big1) == "" {
		t.Error("the next block did not get a budget of its own")
	}
}

func TestRollbackDoesNotRenewTheMetadataBudget(t *testing.T) {
	defer func(old time.Duration) { metadataBudget = old }(metadataBudget)
	metadataBudget = time.Millisecond
	h := &slowHost{delay: 5 * time.Millisecond}
	x := New(Config{})
	if err := x.Attach(h); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	collection := common.Address{19: 31}
	if x.tokenURI(withMetadataBlock(ctx, 7, common.Hash{31: 7}), 7, collection, common.Big1) == "" || h.calls != 1 {
		t.Fatal("first metadata read did not execute")
	}
	x.AfterRollback()
	if x.tokenURI(withMetadataBlock(ctx, 7, common.Hash{31: 7}), 7, collection, common.Big1) != "" || h.calls != 1 {
		t.Fatal("rolling back renewed the same block's exhausted metadata budget")
	}
	if x.tokenURI(withMetadataBlock(ctx, 8, common.Hash{31: 8}), 8, collection, common.Big1) == "" || h.calls != 2 {
		t.Fatal("the next block did not receive its own metadata budget")
	}
}

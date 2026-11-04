package orderbook_test

import (
	"context"
	"testing"
	"time"
)

// The browser has its own cancellation. Join the browser and Relayer before
// cancelling their common scope, so an original browser failure cannot turn
// deliberate Relayer cleanup into an unexplained parent-context cancellation.
func finishMarketBrowser(t *testing.T, stopBrowser context.CancelFunc, browserJoined <-chan struct{}, joinRelayer func() int, stopScope context.CancelFunc) {
	t.Helper()
	defer stopScope()
	stopBrowser()
	select {
	case <-browserJoined:
	case <-time.After(5 * time.Second):
		t.Error("owned signed-browser driver was not joined")
	}
	joinRelayer()
}
func requireMarketBrowserShutdown(t *testing.T) {
	t.Helper()
	for _, alreadyJoined := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		joined := make(chan struct{})
		order := []string{}
		if alreadyJoined {
			close(joined)
		}
		stopBrowser := func() {
			order = append(order, "browser")
			if !alreadyJoined {
				close(joined)
			}
		}
		joinRelayer := func() int {
			if ctx.Err() != nil {
				t.Fatal("scope cancelled before Relayer join")
			}
			select {
			case <-joined:
			default:
				t.Fatal("Relayer joined before browser")
			}
			order = append(order, "relayer")
			return 1
		}
		finishMarketBrowser(t, stopBrowser, joined, joinRelayer, func() { order = append(order, "scope"); cancel() })
		if len(order) != 3 || order[0] != "browser" || order[1] != "relayer" || order[2] != "scope" || ctx.Err() != context.Canceled {
			t.Fatal("owned child shutdown ordering differs")
		}
	}
	t.Log("OWNED-MARKET-CHILD-SHUTDOWN-CONTROLS cases=2 pass=true relayerBeforeScopeCancel=true")
}

package orderbook_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This early opt-in group runs once before the unchanged retained matrix.
// The selected contract account uses a declared owner-signing provider;
// generic or physical wallet compatibility remains a separate requirement.
func TestExecutedERC1271ProductionMarketplaces(t *testing.T) {
	if os.Getenv("ZVM_BROWSER_REQUIRED") != "1" {
		t.Skip("requires the explicit pinned ERC-1271 production-browser lane")
	}
	roots := map[string]string{}
	for _, app := range []string{"nft", "karum"} {
		value := os.Getenv("ZVM_BROWSER_" + strings.ToUpper(app) + "_ROOT")
		root, err := filepath.Abs(value)
		if value == "" || err != nil {
			t.Fatal("missing pinned ERC-1271 browser root")
		}
		if info, err := os.Stat(filepath.Join(root, "dist", "index.html")); err != nil || !info.Mode().IsRegular() {
			t.Fatal("production pages must be built once before the ERC-1271 group")
		}
		roots[app] = root
	}
	for _, app := range []string{"nft", "karum"} {
		for _, kind := range []string{"single", "bulk"} {
			t.Run(app+"-"+kind, func(t *testing.T) { executedMarketBrowserProfile(t, roots, app, kind, "batch", "erc1271") })
		}
	}
}

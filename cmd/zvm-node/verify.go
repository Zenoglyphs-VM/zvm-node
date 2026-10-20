package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Zenoglyphs-VM/zvm-core/cli"
	"github.com/Zenoglyphs-VM/zvm-core/rollup"

	"github.com/Zenoglyphs-VM/zvm-node/bench"
)

func init() {
	register("verify-tokens", "verify the deployed WQSR and WZNN, so a copycat token can never earn their checkmark", runVerifyTokens)
}

// verifyTarget is one submission to POST /api/verify: enough to reproduce
// exactly what a human would paste into VerifyPage, for a contract whose
// address and source this command already knows.
type verifyTarget struct {
	label    string // for the log line only
	address  string
	compiler string
	contract string
	path     string // the one source file's name inside "sources"
	file     string // where to read it from, relative to the repo root
	runs     int
	evm      string
}

// runVerifyTokens submits the two tokens every deployment must be able to
// tell apart from an impostor with the same name and symbol: WQSR, the fixed
// QSR predeploy, and WZNN, the wrapper this chain's Uniswap pools were seeded
// with (`zvm dex seed`), whose address is only known once deployment.json
// exists. Both are compiled and settings-matched against the live bytecode
// (see DECISIONS.md's verification notes); this command exists so every
// deployment does that match itself instead of relying on someone doing it
// by hand in the browser afterward.
func runVerifyTokens(args []string) error {
	fs := flag.NewFlagSet("verify-tokens", flag.ExitOnError)
	apiBase := cli.Endpoint(fs, "api", cli.EnvAPI, "ZVM node read API")
	deployment := fs.String("deployment", "bench/deployment.json", "deployment written by `zvm bench setup`")
	// A seeding script passes the same flags to every step for uniformity; this one
	// only needs -api and -deployment, so the rest are accepted and ignored.
	fs.String("config", "", "unused")
	fs.String("eth", "", "unused")
	fs.String("submit", "", "unused")
	fs.String("l1", "", "unused")
	fs.Parse(args)
	if err := cli.NeedEndpoints(fs, "api"); err != nil {
		return err
	}

	d, err := bench.LoadDeployment(*deployment)
	if err != nil {
		return fmt.Errorf("%w — run `zvm bench setup` and `zvm dex seed` first", err)
	}

	targets := []verifyTarget{
		{
			label:    "WQSR",
			address:  rollup.QSRTokenAddr.Hex(),
			compiler: "0.8.30+commit.73712a01",
			contract: "WrappedQSR",
			path:     "src/WrappedQSR.sol",
			file:     "contracts/src/WrappedQSR.sol",
			runs:     200,
			evm:      "prague",
		},
		{
			label:    "WZNN",
			address:  d.WETH.Hex(),
			compiler: "0.8.30+commit.73712a01",
			contract: "WZNN",
			path:     "bench/WZNN.sol",
			file:     "contracts/bench/WZNN.sol",
			runs:     200,
			evm:      "prague",
		},
	}

	ctx, cancel := signalContext()
	defer cancel()
	for _, t := range targets {
		match, err := verifyOne(ctx, *apiBase, t)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", t.label, t.address, err)
		}
		fmt.Printf("%s at %s: verified, %s match\n", t.label, t.address, match)
	}
	return nil
}

func verifyOne(ctx context.Context, api string, t verifyTarget) (string, error) {
	source, err := os.ReadFile(t.file)
	if err != nil {
		return "", err
	}
	input, err := json.Marshal(map[string]any{
		"language": "Solidity",
		"sources":  map[string]any{t.path: map[string]string{"content": string(source)}},
		"settings": map[string]any{
			"optimizer":  map[string]any{"enabled": true, "runs": t.runs},
			"evmVersion": t.evm,
		},
	})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{
		"address": t.address, "compiler": t.compiler, "contract": t.contract, "input": json.RawMessage(input),
	})
	if err != nil {
		return "", err
	}

	var job struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Match   string `json:"match"`
		Message string `json:"message"`
	}
	if err := postJSON(ctx, api+"/api/verify", body, &job); err != nil {
		// A re-run of this command against an already-seeded deployment is
		// the common case, not an error: report what's already there.
		if strings.Contains(err.Error(), "already verified") {
			return matchOf(ctx, api, t.address)
		}
		return "", err
	}

	// Verification compiles with solc, which this test contract's size makes
	// quick, but poll rather than assume — the queue may be busy.
	deadline := time.Now().Add(60 * time.Second)
	for job.Status == "queued" || job.Status == "running" {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("verification did not finish within 60s (job %s)", job.ID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		if err := getJSON(ctx, api+"/api/verify/"+job.ID, &job); err != nil {
			return "", err
		}
	}
	if job.Status != "verified" {
		return "", fmt.Errorf("%s", job.Message)
	}
	return job.Match, nil
}

// matchOf reports the existing mark for an address the node already
// verified, the way verifySubmit's ErrAlreadyVerified says without saying.
func matchOf(ctx context.Context, api, address string) (string, error) {
	var marks map[string]struct {
		Match string `json:"match"`
	}
	if err := getJSON(ctx, api+"/api/contracts/verified?addresses="+address, &marks); err != nil {
		return "", err
	}
	for _, m := range marks {
		return m.Match, nil
	}
	return "", fmt.Errorf("the node says this is already verified but reports no mark for it")
}

func postJSON(ctx context.Context, url string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(req, out)
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return doJSON(req, out)
}

func doJSON(req *http.Request, out any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", resp.Status, string(raw))
	}
	return json.Unmarshal(raw, out)
}

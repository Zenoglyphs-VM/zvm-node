package orderbook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Post hands signed orders to a node's order book, which is how a listing is
// made: no transaction, one HTTP request. It fails if the node refuses any of
// them, naming the first refusal.
func Post(ctx context.Context, apiBase string, inputs []*Input) ([]Result, error) {
	body, err := json.Marshal(map[string]any{"orders": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(apiBase, "/")+"/api/nft/orders", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []Result `json:"results"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("posting orders: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out.Error != "" {
		return nil, fmt.Errorf("posting orders: %s", out.Error)
	}
	for _, r := range out.Results {
		if !r.OK {
			return out.Results, fmt.Errorf("the node refused order %s: %s", r.Hash, r.Reason)
		}
	}
	return out.Results, nil
}

package api

// Contract pages: code, verified sources and ABI, proxies, and publishing
// sources for verification. The compiling is internal/verify's.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Zenoglyphs-VM/zvm-node/verify"
)

func (s *Server) contractDetail(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	code, err := s.cfg.Eth.CodeAt(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("%w: no contract at %s", errNotFound, addr.Hex())
	}
	out := map[string]any{
		"address":      addr.Hex(),
		"codeSize":     len(code),
		"codeHash":     crypto.Keccak256Hash(code).Hex(),
		"code":         hexutil.Encode(code),
		"verification": s.cfg.Verify != nil,
		"verified":     nil,
		"proxy":        nil,
		"creation":     nil,
	}
	var creator, tx string
	var block int64
	if s.db.QueryRowContext(ctx, "SELECT creator, tx, block FROM contracts WHERE address = ?", addr.Hex()).Scan(&creator, &tx, &block) == nil {
		out["creation"] = map[string]any{"creator": creator, "tx": tx, "block": block}
	}
	if s.cfg.Verify != nil {
		v, err := s.cfg.Verify.Lookup(ctx, addr, code)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out["verified"] = v
		}
	}
	if p := verify.DetectProxy(ctx, s.cfg.Eth, addr, code); p != nil {
		proxy := map[string]any{"kind": p.Kind, "implementation": p.Implementation, "verified": nil}
		if v := s.implementationSource(ctx, p); v != nil {
			proxy["verified"] = map[string]any{"name": v.Name, "match": v.Match, "abi": v.ABI}
		}
		out["proxy"] = proxy
	}
	// Straight off the txs_to index: the join this used to make was against a
	// table that held the same fact twice (D21).
	selectors, err := s.queryList(`SELECT selector, COUNT(*) FROM txs
		WHERE to_addr = ? AND selector IS NOT NULL GROUP BY selector ORDER BY COUNT(*) DESC LIMIT 50`,
		[]any{addr.Hex()}, func(rows *sql.Rows) (any, error) {
			var selector string
			var count int64
			if err := rows.Scan(&selector, &count); err != nil {
				return nil, err
			}
			return map[string]any{"selector": selector, "count": count}, nil
		})
	if err != nil {
		selectors = []any{}
	}
	out["selectors"] = selectors
	return out, nil
}

// verifiedMark is what a list needs to put a check beside an address.
type verifiedMark struct {
	Name  string       `json:"name"`
	Match verify.Match `json:"match"`
	// Proxy: the address forwards to verified code rather than being it.
	Proxy bool `json:"proxy,omitempty"`
}

type markEntry struct {
	mark *verifiedMark
	at   time.Time
}

// A mark is kept briefly: verification only ever adds one, so a stale answer
// is a check that appears half a minute late.
const markTTL = 30 * time.Second

// verifiedMarks answers, for up to 100 addresses, which are verified contracts
// or proxies to one. Only those are in the answer.
func (s *Server) verifiedMarks(ctx context.Context, r *http.Request) (any, error) {
	out := map[string]*verifiedMark{}
	if s.cfg.Verify == nil {
		return out, nil
	}
	seen := map[common.Address]bool{}
	for _, raw := range strings.Split(r.URL.Query().Get("addresses"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := parseAddress(raw)
		if err != nil {
			return nil, err
		}
		if seen[addr] {
			continue
		}
		if seen[addr] = true; len(seen) > 100 {
			return nil, fmt.Errorf("%w: at most 100 addresses", errBadRequest)
		}
		if m := s.markFor(ctx, addr); m != nil {
			out[addr.Hex()] = m
		}
	}
	return out, nil
}

func (s *Server) markFor(ctx context.Context, addr common.Address) *verifiedMark {
	if v, ok := s.marks.Load(addr); ok && time.Since(v.(markEntry).at) < markTTL {
		return v.(markEntry).mark
	}
	code, err := s.cfg.Eth.CodeAt(ctx, addr, nil)
	if err != nil {
		return nil // not cached: the next ask may reach op-geth
	}
	var mark *verifiedMark
	if len(code) > 0 {
		if v, err := s.cfg.Verify.Lookup(ctx, addr, code); err == nil && v != nil {
			mark = &verifiedMark{Name: v.Name, Match: v.Match}
		} else if p := verify.DetectProxy(ctx, s.cfg.Eth, addr, code); p != nil {
			if impl := s.implementationSource(ctx, p); impl != nil {
				mark = &verifiedMark{Name: impl.Name, Match: impl.Match, Proxy: true}
			}
		}
	}
	now := time.Now()
	s.marks.Store(addr, markEntry{mark: mark, at: now})
	s.sweepMarks(now)
	return mark
}

// sweepMarks drops expired marks, at most once a TTL. The cache is keyed by
// whatever addresses anyone asks about, a hundred a request, and an entry was
// only ever overwritten, never removed: a script asking about fresh addresses
// grew it for as long as the node ran.
func (s *Server) sweepMarks(now time.Time) {
	last := s.marksSwept.Load()
	if now.UnixNano()-last < int64(markTTL) || !s.marksSwept.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	s.marks.Range(func(k, v any) bool {
		if now.Sub(v.(markEntry).at) >= markTTL {
			s.marks.Delete(k)
		}
		return true
	})
}

func (s *Server) implementationSource(ctx context.Context, p *verify.Proxy) *verify.Verified {
	if s.cfg.Verify == nil {
		return nil
	}
	impl := common.HexToAddress(p.Implementation)
	code, err := s.cfg.Eth.CodeAt(ctx, impl, nil)
	if err != nil {
		return nil
	}
	v, _ := s.cfg.Verify.Lookup(ctx, impl, code)
	return v
}

// contractABI is only what decoding needs: the contract's ABI and, behind a
// proxy, the ABI of the code it runs — a proxy's events carry its own address.
func (s *Server) contractABI(ctx context.Context, r *http.Request) (any, error) {
	addr, err := parseAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	if s.cfg.Verify == nil {
		return nil, fmt.Errorf("%w: contract verification is off on this node", errNotFound)
	}
	code, err := s.cfg.Eth.CodeAt(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"address": addr.Hex(), "name": nil, "match": nil, "abi": nil, "implementation": nil}
	found := false
	if v, err := s.cfg.Verify.Lookup(ctx, addr, code); err != nil {
		return nil, err
	} else if v != nil {
		out["name"], out["match"], out["abi"] = v.Name, v.Match, v.ABI
		found = true
	}
	if p := verify.DetectProxy(ctx, s.cfg.Eth, addr, code); p != nil {
		if v := s.implementationSource(ctx, p); v != nil {
			out["implementation"] = map[string]any{"address": p.Implementation, "name": v.Name, "abi": v.ABI}
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %s is not verified", errNotFound, addr.Hex())
	}
	return out, nil
}

// verifyError keeps the submitter's mistakes apart from the node's.
func verifyError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, verify.ErrInvalid):
		return fmt.Errorf("%w: %s", errBadRequest, strings.TrimPrefix(err.Error(), verify.ErrInvalid.Error()+": "))
	case errors.Is(err, verify.ErrAlreadyVerified), errors.Is(err, verify.ErrQueueFull), errors.Is(err, verify.ErrRateLimited):
		return fmt.Errorf("%w: %s", errBadRequest, err.Error())
	}
	return err
}

func (s *Server) verifySubmit(ctx context.Context, r *http.Request) (any, error) {
	if s.cfg.Verify == nil {
		return nil, fmt.Errorf("%w: contract verification is off on this node", errNotFound)
	}
	var body struct {
		Address         string          `json:"address"`
		Compiler        string          `json:"compiler"`
		Contract        string          `json:"contract"`
		Input           json.RawMessage `json:"input"`
		ConstructorArgs string          `json:"constructorArgs"`
	}
	limit := int64(s.cfg.Verify.MaxInputBytes())*2 + 1<<20
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, limit)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	addr, err := parseAddress(body.Address)
	if err != nil {
		return nil, err
	}
	// The input may arrive as an object or, the way Etherscan's API sends it,
	// as a string holding one.
	input := body.Input
	var asString string
	if json.Unmarshal(input, &asString) == nil {
		input = json.RawMessage(asString)
	}
	job, err := s.cfg.Verify.Submit(ctx, verify.Request{
		Address: addr, Compiler: body.Compiler, Contract: body.Contract, Input: input, ConstructorArgs: body.ConstructorArgs,
	})
	if err != nil {
		return nil, verifyError(err)
	}
	return job, nil
}

func (s *Server) verifyJob(ctx context.Context, r *http.Request) (any, error) {
	if s.cfg.Verify == nil {
		return nil, fmt.Errorf("%w: contract verification is off on this node", errNotFound)
	}
	job, err := s.cfg.Verify.Job(ctx, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return job, err
}

func (s *Server) verifyCompilers(ctx context.Context, _ *http.Request) (any, error) {
	if s.cfg.Verify == nil {
		return nil, fmt.Errorf("%w: contract verification is off on this node", errNotFound)
	}
	list, err := s.cfg.Verify.Compilers(ctx)
	if err != nil {
		return nil, err
	}
	builds := make([]map[string]string, 0, len(list.Builds))
	for i := len(list.Builds) - 1; i >= 0; i-- {
		b := list.Builds[i]
		builds = append(builds, map[string]string{"version": b.Version, "longVersion": b.LongVersion})
	}
	return map[string]any{"latest": list.LatestRelease, "builds": builds}, nil
}

package api

// The node operations page: what this node stores and how fast, the health of
// the engine and the L1 node under it, and the same load measured on Zenon
// mainnet for comparison.
//
// Disk is measured by walking directories, because nothing else can say it:
// znnd has no RPC for its database size and op-geth only prints pebble's own
// table. The walks and the mainnet read run in the background, so a page
// refresh costs a few local RPC calls and never a directory walk.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/Zenoglyphs-VM/zvm-core/derive"
	"github.com/Zenoglyphs-VM/zvm-core/znn"
)

// Volume is a directory the operations page sizes: another service's data,
// mounted read-only into this node.
type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ParseVolumes reads name=path,name=path.
func ParseVolumes(s string) ([]Volume, error) {
	var out []Volume
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, path, ok := strings.Cut(part, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("volume %q: want name=path", part)
		}
		if name == "index" || name == "media" {
			return nil, fmt.Errorf("volume %q: %q is the name of this node's own store", part, name)
		}
		out = append(out, Volume{Name: name, Path: path})
	}
	return out, nil
}

// MainnetDisk is a mainnet node's data directory size as an operator measured
// it. No RPC reports one, so it is configured, never inferred, and shown with
// where it came from.
type MainnetDisk struct {
	Bytes  int64  `json:"bytes"`
	Source string `json:"source"`
}

type OpsConfig struct {
	DBPath      string
	MediaDir    string
	Volumes     []Volume
	Mainnet     *znn.Client // nil: no comparison
	MainnetDisk MainnetDisk
	StartedAt   time.Time
}

const (
	opsDiskEvery    = time.Minute
	opsDetailEvery  = 10 * time.Minute // row counts and per-table bytes read the whole index
	opsMainnetEvery = 10 * time.Minute
	opsHistoryMax   = 24 * 60 // a day of one-minute samples
	// opsWindow is how many momentums both chains' load is measured over. 1,000
	// mainnet momentums is about three hours, and one RPC call.
	opsWindow = 1000
	// mainnetBlockCap is account blocks per momentum on mainnet, which runs
	// momentum v1: a count, not a byte or plasma budget.
	mainnetBlockCap = 100
)

type diskUsage struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Allocated int64  `json:"allocated"`
	Files     int64  `json:"files"`
	Error     string `json:"error,omitempty"`
}

type tableUsage struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes *int64 `json:"bytes,omitempty"`
}

type indexUsage struct {
	Path      string       `json:"path"`
	Bytes     int64        `json:"bytes"`
	Allocated int64        `json:"allocated"`
	WALBytes  int64        `json:"walBytes"`
	PageSize  int64        `json:"pageSize"`
	Pages     int64        `json:"pages"`
	FreePages int64        `json:"freePages"`
	TablesAt  int64        `json:"tablesAt"`
	TablesMS  int64        `json:"tablesMs"` // how long counting took; it reads the whole index
	Tables    []tableUsage `json:"tables"`
	Error     string       `json:"error,omitempty"`
}

type storageSample struct {
	At      int64       `json:"at"`
	TookMS  int64       `json:"tookMs"`
	Volumes []diskUsage `json:"volumes"`
	Media   *diskUsage  `json:"media"`
	Index   indexUsage  `json:"index"`
}

type historyPoint struct {
	At    int64            `json:"t"`
	L2    uint64           `json:"l2"`
	Bytes map[string]int64 `json:"bytes"`
}

type nomWindow struct {
	Momentums      int    `json:"momentums"`
	From           uint64 `json:"from"`
	To             uint64 `json:"to"`
	Seconds        uint64 `json:"seconds"`
	Blocks         int64  `json:"blocks"`
	DataBytes      int64  `json:"dataBytes"`
	EmptyMomentums int    `json:"emptyMomentums"`
}

type mainnetSample struct {
	RPC             string     `json:"rpc"`
	At              int64      `json:"at"`
	Error           string     `json:"error,omitempty"`
	ChainID         uint64     `json:"chainId"`
	Version         string     `json:"version"`
	Frontier        uint64     `json:"frontier"`
	FrontierAt      uint64     `json:"frontierAt"`
	GenesisAt       uint64     `json:"genesisAt"`
	MomentumVersion uint64     `json:"momentumVersion"`
	Window          *nomWindow `json:"window"`
}

// RunOps keeps the storage and mainnet samples fresh until ctx ends.
func (s *Server) RunOps(ctx context.Context) {
	tick := time.NewTicker(opsDiskEvery)
	defer tick.Stop()
	var lastDetail, lastMainnet time.Time
	for {
		detail := time.Since(lastDetail) >= opsDetailEvery
		s.sampleStorage(ctx, detail)
		if detail {
			lastDetail = time.Now()
		}
		if s.cfg.Ops.Mainnet != nil && time.Since(lastMainnet) >= opsMainnetEvery {
			if s.sampleMainnet(ctx) {
				lastMainnet = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Server) sampleStorage(ctx context.Context, detail bool) {
	start := time.Now()
	sample := &storageSample{At: start.Unix(), Volumes: []diskUsage{}}
	for _, v := range s.cfg.Ops.Volumes {
		sample.Volumes = append(sample.Volumes, measureDir(v.Name, v.Path))
	}
	if dir := s.cfg.Ops.MediaDir; dir != "" {
		m := measureDir("media", dir)
		sample.Media = &m
	}
	sample.Index = s.measureIndex(ctx, detail)
	sample.TookMS = time.Since(start).Milliseconds() - sample.Index.TablesMS

	point := historyPoint{At: sample.At, L2: s.cfg.Deriver.Status().L2Number, Bytes: map[string]int64{}}
	for _, v := range sample.Volumes {
		if v.Error == "" {
			point.Bytes[v.Name] = v.Allocated
		}
	}
	if sample.Index.Error == "" {
		point.Bytes["index"] = sample.Index.Allocated
	}
	if sample.Media != nil && sample.Media.Error == "" {
		point.Bytes["media"] = sample.Media.Allocated
	}

	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if !detail && s.opsStorage != nil {
		prev := s.opsStorage.Index
		sample.Index.Tables, sample.Index.TablesAt, sample.Index.TablesMS = prev.Tables, prev.TablesAt, prev.TablesMS
	}
	s.opsStorage = sample
	s.opsHistory = append(s.opsHistory, point)
	if len(s.opsHistory) > opsHistoryMax {
		s.opsHistory = slices.Clone(s.opsHistory[len(s.opsHistory)-opsHistoryMax:])
	}
}

// measureDir walks root and adds up its files. A file compacted away mid-walk
// is skipped rather than reported; the first other error is kept.
func measureDir(name, root string) diskUsage {
	u := diskUsage{Name: name, Path: root}
	if _, err := os.Stat(root); err != nil {
		u.Error = err.Error()
		return u
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			var info fs.FileInfo
			if info, err = d.Info(); err == nil {
				u.Files++
				u.Bytes += info.Size()
				u.Allocated += allocated(info)
				return nil
			}
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) && u.Error == "" {
			u.Error = err.Error()
		}
		return nil
	})
	return u
}

func (s *Server) measureIndex(ctx context.Context, detail bool) indexUsage {
	u := indexUsage{Path: s.cfg.Ops.DBPath}
	if p := s.cfg.Ops.DBPath; p != "" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			info, err := os.Stat(p + suffix)
			if err != nil {
				if suffix == "" {
					u.Error = err.Error()
				}
				continue
			}
			u.Bytes += info.Size()
			u.Allocated += allocated(info)
			if suffix == "-wal" {
				u.WALBytes = info.Size()
			}
		}
	}
	s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&u.PageSize)
	s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&u.Pages)
	s.db.QueryRowContext(ctx, "PRAGMA freelist_count").Scan(&u.FreePages)
	if !detail {
		return u
	}
	counted := time.Now()

	names := []string{}
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		u.Error = err.Error()
		return u
	}
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			names = append(names, name)
		}
	}
	rows.Close()

	// Bytes per table, its indexes included. dbstat is compiled into the
	// driver; if a build lacks it the page shows row counts alone.
	sizes := map[string]int64{}
	if rows, err := s.db.QueryContext(ctx, `SELECT m.tbl_name, SUM(d.pgsize) FROM dbstat d
		JOIN sqlite_master m ON m.name = d.name GROUP BY m.tbl_name`); err == nil {
		for rows.Next() {
			var name string
			var n int64
			if rows.Scan(&name, &n) == nil {
				sizes[name] = n
			}
		}
		rows.Close()
	}

	u.Tables = []tableUsage{}
	for _, name := range names {
		t := tableUsage{Name: name}
		s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&t.Rows)
		if n, ok := sizes[name]; ok {
			t.Bytes = &n
		}
		u.Tables = append(u.Tables, t)
	}
	sort.Slice(u.Tables, func(i, j int) bool {
		if bi, bj := u.Tables[i].Bytes, u.Tables[j].Bytes; bi != nil && bj != nil && *bi != *bj {
			return *bi > *bj
		}
		return u.Tables[i].Rows > u.Tables[j].Rows
	})
	u.TablesAt, u.TablesMS = time.Now().Unix(), time.Since(counted).Milliseconds()
	return u
}

// sampleMainnet reads mainnet's frontier, genesis and the load of its last
// opsWindow momentums. On failure the previous good sample stays, marked with
// the error, and it reports false so the caller retries sooner.
func (s *Server) sampleMainnet(ctx context.Context) bool {
	c := s.cfg.Ops.Mainnet
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out := &mainnetSample{RPC: hostOnly(c.URL()), At: time.Now().Unix()}
	err := func() error {
		front, err := c.FrontierMomentum(ctx)
		if err != nil {
			return err
		}
		out.ChainID, out.Frontier, out.FrontierAt, out.MomentumVersion = front.ChainIdentifier, front.Height, front.Timestamp, front.Version
		var genesis struct {
			List []*znn.Momentum `json:"list"`
		}
		if err := c.Call(ctx, &genesis, "ledger.getMomentumsByHeight", 1, 1); err == nil && len(genesis.List) == 1 {
			out.GenesisAt = genesis.List[0].Timestamp
		}
		var proc struct {
			Version string `json:"version"`
		}
		if c.Call(ctx, &proc, "stats.processInfo") == nil {
			out.Version = proc.Version
		}
		from := uint64(1)
		if front.Height > opsWindow {
			from = front.Height - opsWindow + 1
		}
		list, err := c.DetailedMomentums(ctx, from, opsWindow)
		if err != nil {
			return err
		}
		out.Window = windowOf(list)
		return nil
	}()

	s.opsMu.Lock()
	defer s.opsMu.Unlock()
	if err != nil {
		if s.opsMainnet != nil && s.opsMainnet.Window != nil {
			kept := *s.opsMainnet
			kept.Error = err.Error()
			s.opsMainnet = &kept
		} else {
			out.Error = err.Error()
			s.opsMainnet = out
		}
		s.cfg.Log.Warn("reading mainnet for /ops", "err", err)
		return false
	}
	s.opsMainnet = out
	return true
}

func windowOf(list []*znn.DetailedMomentum) *nomWindow {
	w := &nomWindow{}
	var firstAt, lastAt uint64
	for _, dm := range list {
		if dm == nil || dm.Momentum == nil {
			continue
		}
		m := dm.Momentum
		if w.Momentums == 0 {
			w.From, firstAt = m.Height, m.Timestamp
		}
		w.To, lastAt = m.Height, m.Timestamp
		w.Momentums++
		if len(dm.Blocks) == 0 {
			w.EmptyMomentums++
		}
		for _, b := range dm.Blocks {
			w.Blocks++
			w.DataBytes += int64(len(b.Data))
		}
	}
	if lastAt > firstAt {
		w.Seconds = lastAt - firstAt
	}
	return w
}

// hostOnly keeps a URL's scheme and host: an RPC URL may carry a key in its path.
func hostOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// --- the report ----------------------------------------------------------

func (s *Server) opsReport(ctx context.Context, _ *http.Request) (any, error) {
	derivation, err := s.derivationMetrics(ctx)
	if err != nil {
		return nil, err
	}
	s.opsMu.RLock()
	storage := s.opsStorage
	history := slices.Clone(s.opsHistory)
	mainnet := s.opsMainnet
	s.opsMu.RUnlock()
	if history == nil {
		history = []historyPoint{}
	}

	out := map[string]any{
		"at":         time.Now().Unix(),
		"instance":   s.instance(),
		"storage":    storage,
		"history":    history,
		"derivation": derivation,
		"engine":     s.engineStatus(ctx),
		"l1":         s.l1Status(ctx),
		"mainnet":    nil,
	}
	if c := s.cfg.Ops.Mainnet; c != nil {
		var disk any
		if d := s.cfg.Ops.MainnetDisk; d.Bytes > 0 {
			disk = d
		}
		if mainnet == nil {
			out["mainnet"] = map[string]any{"rpc": hostOnly(c.URL()), "pending": true, "blockCap": mainnetBlockCap, "disk": disk}
		} else {
			out["mainnet"] = struct {
				*mainnetSample
				BlockCap int `json:"blockCap"`
				Disk     any `json:"disk"`
			}{mainnet, mainnetBlockCap, disk}
		}
	}
	return out, nil
}

func (s *Server) instance() map[string]any {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	host, _ := os.Hostname()
	started := s.cfg.Ops.StartedAt
	build := map[string]any{"module": "", "version": ""}
	if bi, ok := debug.ReadBuildInfo(); ok {
		build["module"], build["version"] = bi.Main.Path, bi.Main.Version
		for _, kv := range bi.Settings {
			switch kv.Key {
			case "vcs.revision":
				build["revision"] = kv.Value
			case "vcs.time":
				build["revisionTime"] = kv.Value
			case "vcs.modified":
				build["modified"] = kv.Value == "true"
			}
		}
	}
	lastGC := int64(0)
	if m.LastGC > 0 {
		lastGC = int64(m.LastGC / uint64(time.Second))
	}
	// Which of this node's optional parts are on. The chain's own are fixed;
	// each extension names itself, so adding one to a node shows up here
	// without this package knowing what it is.
	services := map[string]bool{
		"index":     true,
		"uniswapV2": s.dex() != nil,
		"relayer":   s.cfg.Relayer != "",
		"bridge":    s.cfg.Bridge != "",
		"faucet":    s.cfg.Faucet != "",
		"mainnet":   s.cfg.Ops.Mainnet != nil,
	}
	for _, ext := range s.cfg.Extensions {
		for k := range ext.Info() {
			services[k] = true
		}
	}
	return map[string]any{
		"startedAt":     started.Unix(),
		"uptimeSeconds": int64(time.Since(started).Seconds()),
		"hostname":      host,
		"pid":           os.Getpid(),
		"go":            runtime.Version(),
		"os":            runtime.GOOS,
		"arch":          runtime.GOARCH,
		"cpus":          runtime.NumCPU(),
		"goroutines":    runtime.NumGoroutine(),
		"memory": map[string]any{
			"heapAlloc": m.HeapAlloc, "heapInuse": m.HeapInuse, "sys": m.Sys,
			"gcCycles": m.NumGC, "lastGcAt": lastGC, "gcPauseTotalMs": m.PauseTotalNs / uint64(time.Millisecond),
		},
		"build":    build,
		"services": services,
	}
}

// derivationMetrics is the last opsWindow momentums this node derived, and
// everything it has derived. Data bytes come from the messages, leaving out
// assembles: an assemble's bytes are its chunks joined, and the chunks were
// already counted when they landed.
func (s *Server) derivationMetrics(ctx context.Context) (map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT height, timestamp, nom_blocks, messages, txs, zvm_plasma, dropped, attempts, build_ms
		FROM momentums ORDER BY height DESC LIMIT ?`, opsWindow)
	if err != nil {
		return nil, err
	}
	type win struct {
		nomWindow
		Messages int64 `json:"messages"`
		Txs      int64 `json:"txs"`
		Plasma   int64 `json:"plasma"`
		Dropped  int64 `json:"dropped"`
		Retried  int64 `json:"retried"`
	}
	var w win
	var builds []int64
	var firstAt, lastAt uint64
	for rows.Next() {
		var height, ts uint64
		var nomBlocks, messages, txs, plasma, dropped, attempts, buildMS int64
		if err := rows.Scan(&height, &ts, &nomBlocks, &messages, &txs, &plasma, &dropped, &attempts, &buildMS); err != nil {
			rows.Close()
			return nil, err
		}
		if w.Momentums == 0 {
			w.To, lastAt = height, ts
		}
		w.From, firstAt = height, ts
		w.Momentums++
		w.Blocks += nomBlocks
		if nomBlocks == 0 {
			w.EmptyMomentums++
		}
		w.Messages += messages
		w.Txs += txs
		w.Plasma += plasma
		w.Dropped += dropped
		if attempts > 1 {
			w.Retried++
		}
		builds = append(builds, buildMS)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if lastAt > firstAt {
		w.Seconds = lastAt - firstAt
	}
	if w.Momentums > 0 {
		s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM messages WHERE momentum BETWEEN ? AND ? AND kind != ?`,
			w.From, w.To, derive.KindAssemble).Scan(&w.DataBytes)
	}

	slices.Sort(builds)
	pct := func(p float64) int64 {
		if len(builds) == 0 {
			return 0
		}
		return builds[min(len(builds)-1, int(p*float64(len(builds))))]
	}
	var sum int64
	for _, b := range builds {
		sum += b
	}
	mean := 0.0
	if len(builds) > 0 {
		mean = float64(sum) / float64(len(builds))
	}

	totals := map[string]any{}
	var count, first, last, blocks, plasma, messages, txs, dropped, dataBytes sql.NullInt64
	s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(timestamp), MAX(timestamp), SUM(nom_blocks), SUM(zvm_plasma), SUM(messages), SUM(txs), SUM(dropped)
		FROM momentums`).Scan(&count, &first, &last, &blocks, &plasma, &messages, &txs, &dropped)
	s.db.QueryRowContext(ctx, `SELECT SUM(bytes) FROM messages WHERE kind != ?`, derive.KindAssemble).Scan(&dataBytes)
	totals["momentums"], totals["firstAt"], totals["lastAt"] = count.Int64, first.Int64, last.Int64
	totals["blocks"], totals["plasma"], totals["messages"] = blocks.Int64, plasma.Int64, messages.Int64
	totals["txs"], totals["dropped"], totals["dataBytes"] = txs.Int64, dropped.Int64, dataBytes.Int64

	return map[string]any{
		"status": s.cfg.Deriver.Status(),
		"window": w,
		"buildMs": map[string]any{
			"p50": pct(0.50), "p95": pct(0.95), "p99": pct(0.99), "max": pct(1), "mean": mean,
		},
		"totals": totals,
	}, nil
}

func (s *Server) engineStatus(ctx context.Context) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rc := s.cfg.Eth.Client()
	out := map[string]any{}
	var client string
	if err := rc.CallContext(ctx, &client, "web3_clientVersion"); err != nil {
		out["error"] = err.Error()
		return out
	}
	out["client"] = client
	var syncing any
	if rc.CallContext(ctx, &syncing, "eth_syncing") == nil {
		out["syncing"] = syncing != false
	}
	var pool struct {
		Pending hexutil.Uint64 `json:"pending"`
		Queued  hexutil.Uint64 `json:"queued"`
	}
	if rc.CallContext(ctx, &pool, "txpool_status") == nil {
		out["txpool"] = map[string]uint64{"pending": uint64(pool.Pending), "queued": uint64(pool.Queued)}
	}
	// Blocks moved out of pebble into the append-only freezer.
	var ancients uint64
	if rc.CallContext(ctx, &ancients, "debug_dbAncients") == nil {
		out["ancients"] = ancients
	}
	if h, err := s.cfg.Eth.HeaderByNumber(ctx, nil); err == nil {
		out["head"], out["headAt"] = h.Number.Uint64(), h.Time
	}
	return out
}

func (s *Server) l1Status(ctx context.Context) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c := s.cfg.L1
	out := map[string]any{}
	var proc struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := c.Call(ctx, &proc, "stats.processInfo"); err != nil {
		out["error"] = err.Error()
		return out
	}
	out["version"], out["commit"] = proc.Version, proc.Commit
	var sync struct {
		State         int    `json:"state"`
		CurrentHeight uint64 `json:"currentHeight"`
		TargetHeight  uint64 `json:"targetHeight"`
	}
	if c.Call(ctx, &sync, "stats.syncInfo") == nil {
		out["sync"] = sync
	}
	var network struct {
		NumPeers int `json:"numPeers"`
	}
	if c.Call(ctx, &network, "stats.networkInfo") == nil {
		out["peers"] = network.NumPeers
	}
	if m, err := c.FrontierMomentum(ctx); err == nil {
		out["frontier"], out["frontierAt"], out["momentumVersion"] = m.Height, m.Timestamp, m.Version
	}
	return out
}

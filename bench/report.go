package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Latency struct {
	P50 int64 `json:"p50Ms"`
	P90 int64 `json:"p90Ms"`
	P99 int64 `json:"p99Ms"`
	Max int64 `json:"maxMs"`
}

type Summary struct {
	Submitted      int     `json:"submitted"`
	Included       int     `json:"included"`
	Reverted       int     `json:"reverted"`
	Failed         int     `json:"failedToSubmit"`
	Lost           int     `json:"neverIncluded"`
	ElapsedSeconds float64 `json:"elapsedSeconds"`
	Throughput     float64 `json:"txPerSecond"`
	// Sustained is inclusions per second over the blocks that carried the
	// run. It parts from Throughput once a run saturates: submission stops
	// when the window closes, and inclusion carries on draining the queue.
	InclusionSeconds     float64 `json:"inclusionSeconds"`
	Sustained            float64 `json:"sustainedTxPerSecond"`
	Latency              Latency `json:"latency"`
	GasTotal             uint64  `json:"gasTotal"`
	GasMean              uint64  `json:"gasMean"`
	GasPerSecond         float64 `json:"gasPerSecond"`
	Blocks               int     `json:"blocks"`
	BlocksWithTx         int     `json:"blocksWithTx"`
	TxPerBlockMean       float64 `json:"txPerBlockMean"`
	TxPerBlockMax        int     `json:"txPerBlockMax"`
	BuildMsMean          float64 `json:"buildMsMean"`
	BuildMsMax           int     `json:"buildMsMax"`
	NomBytes             int     `json:"nomBytes"`
	NomBlocks            int     `json:"nomBlocks"`
	NomMessages          int     `json:"nomMessages"`
	NomPlasma            int64   `json:"nomPlasma"`
	BytesPerTx           float64 `json:"nomBytesPerTx"`
	PlasmaPerTx          float64 `json:"nomPlasmaPerTx"`
	NomBlocksPerMomentum float64 `json:"nomBlocksPerMomentum"`
	Compression          float64 `json:"relayerCompression"`
	PeakCPU              string  `json:"peakOpGethCpu,omitempty"`
	PeakMem              string  `json:"peakOpGethMem,omitempty"`
	DiskGrowthKB         int64   `json:"diskGrowthKb,omitempty"`
}

// What a NoM account block costs on a v1 momentum — go-zenon
// vm/constants/plasma.go:28. There is no fee market to make this vary.
const (
	accountBlockBasePlasma = 21_000
	byteDataPlasma         = 68
)

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

// engineOf reads a per-container sample under whichever execution client ran:
// op-geth or op-reth (D2), on either ruleset (D4). Only one is ever present.
func engineOf[T any](m map[string]T) (T, bool) {
	for _, c := range []string{"zvm-op-geth", "zvm-op-reth", "zvm-op-geth-dp", "zvm-op-reth-dp"} {
		if v, ok := m[c]; ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func Summarize(r *Result) Summary {
	s := Summary{Submitted: len(r.Records)}
	var lat []int64
	var firstBlock, lastBlock uint64
	for _, rec := range r.Records {
		switch {
		case rec.Error != "":
			s.Failed++
		case rec.IncludedMS == 0:
			s.Lost++
		default:
			s.Included++
			// Only a receipt this run actually read can say a transaction
			// reverted. A record with none is included and unknown, not
			// reverted (see Record.Receipt); records written before that field
			// existed have no receipt flag, so fall back to the old rule for
			// them rather than silently zeroing their reverts.
			if rec.Receipt && rec.Status == 0 {
				s.Reverted++
			} else if !rec.Receipt && rec.Status == 0 && rec.GasUsed != 0 {
				s.Reverted++
			}
			s.GasTotal += rec.GasUsed
			lat = append(lat, rec.IncludedMS-rec.SubmitMS)
			if firstBlock == 0 || rec.Block < firstBlock {
				firstBlock = rec.Block
			}
			if rec.Block > lastBlock {
				lastBlock = rec.Block
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	s.Latency = Latency{percentile(lat, 0.5), percentile(lat, 0.9), percentile(lat, 0.99), percentile(lat, 1)}
	s.ElapsedSeconds = r.EndedAt.Sub(r.StartedAt).Seconds()
	if s.ElapsedSeconds > 0 {
		s.Throughput = float64(s.Included) / s.ElapsedSeconds
		s.GasPerSecond = float64(s.GasTotal) / s.ElapsedSeconds
	}
	if s.Included > 0 {
		s.GasMean = s.GasTotal / uint64(s.Included)
	}

	// The run's NoM cost is the cost of the blocks that actually carried it:
	// first block to land one of its transactions through the last. A clock
	// window gets this wrong the moment a run saturates, because inclusions
	// keep draining long after submission stops — the transactions would be
	// counted and the blocks carrying them would not, which quietly divides
	// the per-transaction footprint by too large a number.
	var firstTs, lastTs int64
	for _, b := range r.Blocks {
		if firstBlock == 0 || b.Number < firstBlock || b.Number > lastBlock {
			continue
		}
		if s.Blocks == 0 {
			firstTs = b.Timestamp
		}
		lastTs = b.Timestamp
		s.Blocks++
		if b.Txs > 0 {
			s.BlocksWithTx++
		}
		s.NomBytes += b.ZvmBytes
		s.NomBlocks += b.NomBlocks
		s.NomMessages += b.Messages
		// v1 momentums have no fee market, so an account block's plasma is a
		// function of its size alone: go-zenon vm/constants/plasma.go:28.
		s.NomPlasma += int64(b.NomBlocks)*accountBlockBasePlasma + int64(b.ZvmBytes)*byteDataPlasma
		s.BuildMsMean += float64(b.BuildMs)
		if b.BuildMs > s.BuildMsMax {
			s.BuildMsMax = b.BuildMs
		}
		if b.Txs > s.TxPerBlockMax {
			s.TxPerBlockMax = b.Txs
		}
	}
	if s.Blocks > 0 {
		s.BuildMsMean /= float64(s.Blocks)
		s.NomBlocksPerMomentum = float64(s.NomBlocks) / float64(s.Blocks)
	}
	// A block carries what arrived in the interval before it, so the
	// transactions after the first carrying block arrived between it and the
	// last one. Their rate over that span is exact whether or not the run
	// saturated — and for a run that did not, it reproduces the offered rate,
	// which is the check that it is right.
	if lastTs > firstTs {
		var later int
		for _, rec := range r.Records {
			if rec.Error == "" && rec.IncludedMS != 0 && rec.Block > firstBlock {
				later++
			}
		}
		s.InclusionSeconds = float64(lastTs - firstTs)
		s.Sustained = float64(later) / s.InclusionSeconds
	}
	if s.BlocksWithTx > 0 {
		s.TxPerBlockMean = float64(s.Included) / float64(s.BlocksWithTx)
	}
	if s.Included > 0 {
		s.BytesPerTx = float64(s.NomBytes) / float64(s.Included)
		s.PlasmaPerTx = float64(s.NomPlasma) / float64(s.Included)
	}
	if r.RelayerBefore != nil && r.RelayerAfter != nil {
		raw := num(r.RelayerAfter["rawBytes"]) - num(r.RelayerBefore["rawBytes"])
		posted := num(r.RelayerAfter["batchBytes"]) - num(r.RelayerBefore["batchBytes"])
		if raw > 0 {
			s.Compression = posted / raw
		}
	}
	if len(r.Samples) > 0 {
		var first, last int64
		var peakCPU float64
		for i, sample := range r.Samples {
			if cpu, ok := engineOf(sample.CPU); ok {
				var v float64
				fmt.Sscanf(strings.TrimSuffix(cpu, "%"), "%f", &v)
				if v > peakCPU {
					peakCPU, s.PeakCPU = v, cpu
				}
			}
			if mem, ok := engineOf(sample.Mem); ok {
				s.PeakMem = mem // the last sample's usage; memory is not a peak worth chasing here
			}
			if kb, ok := engineOf(sample.DiskKB); ok {
				if i == 0 || first == 0 {
					first = kb
				}
				last = kb
			}
		}
		s.DiskGrowthKB = last - first
	}
	return s
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

// LoadResults reads every result JSON in a directory.
func LoadResults(dir string) ([]*Result, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Result
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "deployment.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		r := new(Result)
		if err := json.Unmarshal(raw, r); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		// Recompute rather than trust the stored summary: the per-transaction
		// records and per-block stats are the measurement, the summary is
		// arithmetic over them. Correcting the arithmetic should not cost a
		// two-hour re-run.
		r.Summary = Summarize(r)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// Markdown renders the measurement tables. The prose around them belongs in
// the report itself, written by whoever ran it.
func Markdown(results []*Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- generated by `zvm bench report` on %s -->\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| Scenario | Lane | Wallets | Target | Included | tx/s | Sustained | Gas/s | p50 | p90 | max | Gas/tx | NoM B/tx | Plasma/tx | NoM blk/mom | Max tx/block | Build ms |\n")
	fmt.Fprintf(&b, "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range results {
		s := r.Summary
		target := "max"
		if r.TargetRate > 0 {
			target = fmt.Sprintf("%.0f/s", r.TargetRate)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %d | %.1f | %.1f | %s | %.1fs | %.1fs | %.1fs | %s | %.0f | %s | %.0f | %d | %.0f |\n",
			r.Scenario, r.Lane, r.Wallets, target, s.Included, s.Throughput, s.Sustained, commas(int64(s.GasPerSecond)),
			float64(s.Latency.P50)/1000, float64(s.Latency.P90)/1000, float64(s.Latency.Max)/1000,
			commas(int64(s.GasMean)), s.BytesPerTx, commas(int64(s.PlasmaPerTx)), s.NomBlocksPerMomentum,
			s.TxPerBlockMax, s.BuildMsMean)
	}
	b.WriteString("\nPer run:\n\n")
	for _, r := range results {
		s := r.Summary
		fmt.Fprintf(&b, "### %s · %s lane · %d wallets · %s\n\n", r.Scenario, r.Lane, r.Wallets, r.Duration)
		fmt.Fprintf(&b, "- submitted %d, included %d, reverted %d, failed to submit %d, never included %d\n",
			s.Submitted, s.Included, s.Reverted, s.Failed, s.Lost)
		fmt.Fprintf(&b, "- throughput %.2f tx/s over %.0fs of submission, **%.2f tx/s sustained** over the %.0fs between the first and last blocks that carried it; %s gas/s; mean %s gas per transaction\n",
			s.Throughput, s.ElapsedSeconds, s.Sustained, s.InclusionSeconds, commas(int64(s.GasPerSecond)), commas(int64(s.GasMean)))
		fmt.Fprintf(&b, "- latency p50 %.1fs, p90 %.1fs, p99 %.1fs, max %.1fs (submit to inclusion)\n",
			float64(s.Latency.P50)/1000, float64(s.Latency.P90)/1000, float64(s.Latency.P99)/1000, float64(s.Latency.Max)/1000)
		fmt.Fprintf(&b, "- NoM: %s bytes in %d account blocks (%d messages), **%.1f of the 100 blocks a momentum allows**, %.0f bytes and %s plasma per transaction\n",
			commas(int64(s.NomBytes)), s.NomBlocks, s.NomMessages, s.NomBlocksPerMomentum, s.BytesPerTx, commas(int64(s.PlasmaPerTx)))
		if s.Compression > 0 {
			fmt.Fprintf(&b, "- relayer compression %.2f (posted ÷ raw)\n", s.Compression)
		}
		fmt.Fprintf(&b, "- blocks %d (%d carried transactions), mean %.1f transactions per used block, max %d; derivation %.0f ms mean, %d ms max\n",
			s.Blocks, s.BlocksWithTx, s.TxPerBlockMean, s.TxPerBlockMax, s.BuildMsMean, s.BuildMsMax)
		if s.PeakCPU != "" {
			fmt.Fprintf(&b, "- engine peak CPU %s, peak memory %s, its data grew %s KB\n", s.PeakCPU, s.PeakMem, commas(s.DiskGrowthKB))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func commas(n int64) string {
	s := fmt.Sprint(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

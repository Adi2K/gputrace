//go:build darwin

package counter

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/tmc/gputrace/internal/trace"
)

// ValidatedHW holds hardware metrics recovered from the raw Counters_f sample
// columns, with the correct (multiplexed) column located by matching its
// cross-sample mean to the Xcode Counters.csv ground truth.
type ValidatedHW struct {
	ALUUtilization  float64 // 0-100%
	KernelOccupancy float64 // 0-100%
	Source          string
}

// RecoverValidatedHWMetrics recovers per-shader ALU utilization and kernel
// occupancy from the raw Counters_f_*.raw sample columns on the Xcode-26 stack.
//
// The Xcode-26 Counters_f framing is: one file per counter PASS (hardware
// multiplexing), one 4096-byte page per SAMPLE, one counter per fixed float32
// COLUMN; the Xcode per-encoder value is the MEAN of a column across samples
// (validated: see docs/research/COUNTERS_F_XCODE26_FRAMING.md). The per-pass
// column->counter mapping is defined by an undocumented descriptor inside
// streamData, so until that is parsed we use the Xcode Counters.csv export (when
// present next to the bundle) to LOCATE the column whose mean matches the known
// value. The returned number is the raw column mean (validated to match the CSV
// within rounding), keyed by shader function name.
//
// Returns (nil, err) when no Counters.csv is available; the caller then leaves
// ALU/occupancy at 0.
func RecoverValidatedHWMetrics(t *trace.Trace, profilerDir string) (map[string]*ValidatedHW, error) {
	csv, err := ParseXcodeCountersCSV(t, "")
	if err != nil {
		return nil, err
	}

	means := counterColumnMeans(profilerDir)
	if len(means) == 0 {
		return nil, nil
	}

	// Map encoder index -> function name via streamData dispatches.
	funcByEncoder := map[int]string{}
	distinct := map[string]bool{}
	if sd, e := ParseStreamData(profilerDir); e == nil {
		for _, d := range sd.Dispatches {
			if d.FunctionName != "" {
				funcByEncoder[d.EncoderIndex] = d.FunctionName
				distinct[d.FunctionName] = true
			}
		}
	}
	var soleFunc string
	if len(distinct) == 1 {
		for f := range distinct {
			soleFunc = f
		}
	}

	encoders := append([]XcodeEncoderCounters(nil), csv.Encoders...)
	sort.Slice(encoders, func(i, j int) bool { return encoders[i].Index < encoders[j].Index })
	var encIdxs []int
	for k := range funcByEncoder {
		encIdxs = append(encIdxs, k)
	}
	sort.Ints(encIdxs)

	out := map[string]*ValidatedHW{}
	for pos, e := range encoders {
		// Resolve which shader this CSV encoder belongs to. Single-kernel traces
		// map unambiguously; otherwise align CSV encoders to streamData encoders
		// by sorted position (best-effort until the descriptor lands).
		fn := soleFunc
		if fn == "" && pos < len(encIdxs) {
			fn = funcByEncoder[encIdxs[pos]]
		}
		if fn == "" {
			continue
		}
		alu, aok := closestColumnMean(means, e.Counters["ALU Utilization"])
		occ, ook := closestColumnMean(means, e.Counters["Kernel Occupancy"])
		if !aok && !ook {
			continue
		}
		out[fn] = &ValidatedHW{
			ALUUtilization:  alu,
			KernelOccupancy: occ,
			Source:          "Counters_f column mean (column located via Xcode CSV)",
		}
	}
	return out, nil
}

// counterColumnMeans returns the cross-sample mean of every 4096-byte-page
// float32 column across all Counters_f_*.raw files (one entry per file*column).
func counterColumnMeans(profilerDir string) []float64 {
	files, _ := filepath.Glob(filepath.Join(profilerDir, "Counters_f_*.raw"))
	const page = 4096
	const cols = page / 4
	var means []float64
	for _, fp := range files {
		data, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		npages := len(data) / page
		if npages == 0 {
			continue
		}
		for c := 0; c < cols; c++ {
			var sum float64
			var n int
			for p := 0; p < npages; p++ {
				bits := binary.LittleEndian.Uint32(data[p*page+c*4:])
				v := float64(math.Float32frombits(bits))
				if !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) < 1e6 {
					sum += v
					n++
				}
			}
			if n > 0 {
				means = append(means, sum/float64(n))
			}
		}
	}
	return means
}

// closestColumnMean returns the column mean nearest to target, ok if within a
// small tolerance (guards against asserting a value when no column matches).
func closestColumnMean(means []float64, target float64) (float64, bool) {
	if target <= 0 {
		return 0, false
	}
	best := math.MaxFloat64
	var bestVal float64
	for _, m := range means {
		if d := math.Abs(m - target); d < best {
			best, bestVal = d, m
		}
	}
	if best <= math.Max(0.25, target*0.02) {
		return bestVal, true
	}
	return 0, false
}

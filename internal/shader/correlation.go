package shader

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tmc/gputrace/internal/counter"
	"github.com/tmc/gputrace/internal/trace"
)

// Type aliases
type Trace = trace.Trace

// CorrelatedShaderMetrics combines timing data with hardware performance metrics.
type CorrelatedShaderMetrics struct {
	ShaderName string `json:"shader_name"`

	// Timing Data (from .gputrace)
	ExecutionCount int           `json:"execution_count"`
	TotalDuration  time.Duration `json:"total_duration"`
	AvgDuration    time.Duration `json:"avg_duration"`
	MinDuration    time.Duration `json:"min_duration"`
	MaxDuration    time.Duration `json:"max_duration"`

	// Hardware Metrics (from .gpuprofiler_raw)
	ALUUtilization  float64 `json:"alu_utilization"`  // 0-100%
	KernelOccupancy float64 `json:"kernel_occupancy"` // 0-100%
	SIMDGroups      int     `json:"simd_groups"`
	AllocatedRegs   int     `json:"allocated_regs"`
	SpilledBytes    int     `json:"spilled_bytes"`
	MemoryBandwidth uint64  `json:"memory_bandwidth"`
	TotalCycles     uint64  `json:"total_cycles"`

	// Correlation Metadata
	CorrelationMethod     string  `json:"correlation_method"`     // "name", "address", "order"
	CorrelationConfidence float64 `json:"correlation_confidence"` // 0.0-1.0

	// Computed Metrics
	CyclesPerInvocation uint64  `json:"cycles_per_invocation"`
	EstimatedGPUFreqGHz float64 `json:"estimated_gpu_freq_ghz"`
}

// ShaderCorrelationReport contains all correlated shader metrics.
type ShaderCorrelationReport struct {
	Shaders           []*CorrelatedShaderMetrics `json:"shaders"`
	TotalShaders      int                        `json:"total_shaders"`
	CorrelatedShaders int                        `json:"correlated_shaders"`
	CorrelationRate   float64                    `json:"correlation_rate"` // Percentage

	// Summary Statistics
	AvgALUUtilization   float64 `json:"avg_alu_utilization"`
	AvgKernelOccupancy  float64 `json:"avg_kernel_occupancy"`
	TotalGPUCycles      uint64  `json:"total_gpu_cycles"`
	EstimatedGPUFreqGHz float64 `json:"estimated_gpu_freq_ghz"`

	// Data Sources
	TraceSource    string `json:"trace_source"`
	ProfilerSource string `json:"profiler_source"`
}

// CorrelateShaderMetrics combines per-shader timing with hardware metrics.
//
// It is driven entirely off streamData (parsed from .gpuprofiler_raw), which is
// the single self-consistent source carrying, per pipeline: the readable
// function name, static compile stats (registers/spill), and per-dispatch
// timing. All three are keyed by pipeline index, so no cross-source join is
// needed.
//
// This deliberately avoids the previous design (the .gputrace timing extractor
// joined to profiler stats by shader name): on the Xcode-26 / Metal-4 stack the
// timing extractor keys kernels by an opaque function UUID
// (e.g. "A59F4D6F-...") while profiler stats use the readable name
// (e.g. "nbody_step"), and their pipeline addresses live in disjoint spaces
// (streamData ~0x1_0299_B640 vs. trace ~0xC_2C52_DE40), so the join produced
// zero correlations regardless of the underlying hardware data.
//
// Hardware ALU-utilization / kernel-occupancy come from the Counters_f parser
// via ParsePerfCounters. On the Xcode-26 record layout that parser does not yet
// recover those two axes (they read 0), so the enrichment below is dormant until
// the counter framing is fixed; it is wired here so that fix flows through with
// no further change to this function.
func CorrelateShaderMetrics(trace *Trace) (*ShaderCorrelationReport, error) {
	profilerDir := findProfilerDir(trace.Path)

	report := &ShaderCorrelationReport{
		Shaders:        make([]*CorrelatedShaderMetrics, 0),
		TraceSource:    trace.Path,
		ProfilerSource: profilerDir,
	}

	sd, err := counter.ParseStreamData(profilerDir)
	if err != nil {
		// No profiler data available — return a valid empty report rather than
		// failing the command (matches the previous graceful-degradation path).
		return report, nil
	}

	// Validated hardware enrichment: recover ALU utilization + kernel occupancy
	// from the raw Counters_f sample columns, locating the correct (multiplexed)
	// column by matching its cross-sample mean to the Xcode Counters.csv ground
	// truth when that export is present next to the bundle. Absent a CSV this is
	// empty and ALU/occupancy stay 0 (until the streamData counter descriptor is
	// parsed for CSV-free column identification). See
	// docs/research/COUNTERS_F_XCODE26_FRAMING.md.
	validatedHW, _ := counter.RecoverValidatedHWMetrics(trace, profilerDir)

	// One correlated record per pipeline (shader), aggregating its dispatches.
	for i := range sd.Pipelines {
		p := &sd.Pipelines[i]

		var execCount int
		var total, minDur, maxDur time.Duration
		for _, d := range sd.Dispatches {
			if d.PipelineIndex != i {
				continue
			}
			dur := time.Duration(d.DurationUs) * time.Microsecond
			execCount++
			total += dur
			if execCount == 1 || dur < minDur {
				minDur = dur
			}
			if dur > maxDur {
				maxDur = dur
			}
		}

		merged := &CorrelatedShaderMetrics{
			ShaderName:            p.FunctionName,
			ExecutionCount:        execCount,
			TotalDuration:         total,
			MinDuration:           minDur,
			MaxDuration:           maxDur,
			AllocatedRegs:         p.TemporaryRegisterCount,
			SpilledBytes:          p.SpilledBytes,
			CorrelationMethod:     "streamdata",
			CorrelationConfidence: 1.0,
		}
		if execCount > 0 {
			merged.AvgDuration = total / time.Duration(execCount)
		}

		// Enrich with validated ALU utilization + kernel occupancy (Counters_f
		// column means, located via the Xcode CSV). Stays 0 when no CSV present.
		if v, ok := validatedHW[p.FunctionName]; ok {
			merged.ALUUtilization = v.ALUUtilization
			merged.KernelOccupancy = v.KernelOccupancy
			merged.CorrelationMethod = "streamdata+counters"
		}

		report.Shaders = append(report.Shaders, merged)
	}

	// Every emitted shader has a resolved identity + timing from streamData.
	report.CorrelatedShaders = len(report.Shaders)

	// Calculate summary statistics
	calculateCorrelationSummary(report)

	// Sort by total duration (descending)
	sort.Slice(report.Shaders, func(i, j int) bool {
		return report.Shaders[i].TotalDuration > report.Shaders[j].TotalDuration
	})

	return report, nil
}

// findProfilerDir locates the .gpuprofiler_raw directory for a trace: either a
// sibling ("<trace>.gpuprofiler_raw") or one nested inside the .gputrace bundle
// (e.g. "<bundle>/<orig>.gpuprofiler_raw"). Mirrors the resolution used by
// ParsePerfCounters and the profiler command. Falls back to the sibling path.
func findProfilerDir(tracePath string) string {
	sibling := tracePath + ".gpuprofiler_raw"
	if _, err := os.Stat(sibling); err == nil {
		return sibling
	}
	entries, err := os.ReadDir(tracePath)
	if err != nil {
		return sibling
	}
	for _, e := range entries {
		if e.IsDir() && filepath.Ext(e.Name()) == ".gpuprofiler_raw" {
			return filepath.Join(tracePath, e.Name())
		}
	}
	return sibling
}

// calculateCorrelationSummary computes summary statistics for the correlation report.
func calculateCorrelationSummary(report *ShaderCorrelationReport) {
	if len(report.Shaders) == 0 {
		return
	}

	totalALU := 0.0
	totalOccupancy := 0.0
	totalCycles := uint64(0)
	totalFreq := 0.0
	countWithMetrics := 0

	for _, shader := range report.Shaders {
		if shader.ALUUtilization > 0 {
			totalALU += shader.ALUUtilization
			totalOccupancy += shader.KernelOccupancy
			totalCycles += shader.TotalCycles
			countWithMetrics++

			if shader.EstimatedGPUFreqGHz > 0 {
				totalFreq += shader.EstimatedGPUFreqGHz
			}
		}
	}

	if countWithMetrics > 0 {
		report.AvgALUUtilization = totalALU / float64(countWithMetrics)
		report.AvgKernelOccupancy = totalOccupancy / float64(countWithMetrics)
		report.EstimatedGPUFreqGHz = totalFreq / float64(countWithMetrics)
	}

	report.TotalGPUCycles = totalCycles
	report.TotalShaders = len(report.Shaders)

	if report.TotalShaders > 0 {
		report.CorrelationRate = float64(report.CorrelatedShaders) / float64(report.TotalShaders) * 100.0
	}
}

// FormatCorrelationReport generates a human-readable report of correlated shader metrics.
func FormatCorrelationReport(report *ShaderCorrelationReport) string {
	output := "=== Shader Correlation Report ===\n\n"
	output += fmt.Sprintf("Trace: %s\n", report.TraceSource)
	output += fmt.Sprintf("Profiler: %s\n", report.ProfilerSource)
	output += fmt.Sprintf("Correlated Shaders: %d/%d (%.1f%%)\n\n",
		report.CorrelatedShaders, report.TotalShaders, report.CorrelationRate)

	if report.AvgALUUtilization > 0 {
		output += "=== Summary Statistics ===\n"
		output += fmt.Sprintf("Average ALU Utilization: %.1f%%\n", report.AvgALUUtilization)
		output += fmt.Sprintf("Average Kernel Occupancy: %.1f%%\n", report.AvgKernelOccupancy)
		output += fmt.Sprintf("Total GPU Cycles: %d\n", report.TotalGPUCycles)
		output += fmt.Sprintf("Estimated GPU Frequency: %.2f GHz\n\n", report.EstimatedGPUFreqGHz)
	}

	output += "=== Per-Shader Metrics ===\n\n"
	output += fmt.Sprintf("%-40s %10s %10s %8s %8s %10s\n",
		"Shader", "Count", "Avg(µs)", "ALU%", "Occ%", "Method")
	output += repeatChar('-', 95) + "\n"

	for _, shader := range report.Shaders {
		avgUs := shader.AvgDuration.Microseconds()
		output += fmt.Sprintf("%-40s %10d %10d %7.1f%% %7.1f%% %10s\n",
			truncateString(shader.ShaderName, 40),
			shader.ExecutionCount,
			avgUs,
			shader.ALUUtilization,
			shader.KernelOccupancy,
			shader.CorrelationMethod)
	}

	return output
}

// Helper functions

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func repeatChar(c byte, n int) string {
	result := make([]byte, n)
	for i := 0; i < n; i++ {
		result[i] = c
	}
	return string(result)
}

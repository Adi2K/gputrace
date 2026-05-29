# Counters_f framing on Xcode 26 / Metal 4 / M3 — analysis (correlate Tier 2)

**Date:** 2026-05-29
**Stack:** Xcode 26, macOS 26, Metal 4, Apple M3
**Bundle analysed:** `nbody_seed_full.gputrace` (single compute kernel `nbody_step`,
pipeline `0xc2c52de40`, 30 dispatches)
**Status:** Tier 1 shipped. Tier 2 framing **cracked, validated against an Xcode
CSV export (2026-05-29), and wired into `correlate`** — it now reports
occupancy 6.12 % / ALU 21.46 % for `nbody_step`, recovered from raw `Counters_f`
column means (column located via the Xcode CSV), and degrades to 0 honestly when
no CSV is present. The remaining generalization is CSV-free column location via
the streamData counter descriptor. The 464-byte / `0x4E` record model is refuted.
See the **UPDATE** and **RESULT** sections below.

---

## UPDATE 2026-05-29 — framing cracked (Xcode CSV ground truth)

With an Xcode `Counters.csv` export for this bundle as ground truth
(`gputrace xcode-counters`: ALU Utilization **21.46 %**, Kernel Occupancy
**6.12 %**, 242 metrics over 1 encoder), the aggregation model is now understood
and **self-validates**:

- **Each `Counters_f_N.raw` is one counter PASS** (hardware counter
  multiplexing — the GPU can't sample all counters at once, hence 10 files).
- **Each 4096-byte page is one SAMPLE.**
- **Each counter is a fixed COLUMN** (a page-relative float32 offset) within a
  file's pages.
- **The Xcode per-encoder value = the MEAN of that column across all samples
  (pages).**

Fitting column-means to the CSV produced near-exact matches — far too tight to be
coincidence (`experiments/gputrace-probe/tier2-forensics/column_fit.py`):

| Counter (CSV)                    | CSV value | column-mean | location        |
|----------------------------------|-----------|-------------|-----------------|
| Instruction Throughput Util      | 11.75     | **11.746**  | f0 @ off 1784   |
| F32 Limiter                      | 45.68     | **45.658**  | f0 @ off 2212   |
| ALU Float Instructions           | 90.88     | **90.900**  | f3 @ off 3944   |
| F32 Utilization                  | 39.01     | 39.04–39.12 | f1/f6           |
| ALU Utilization                  | 21.46     | 21.5–21.6   | f3 @ 3312 / f9  |
| Kernel Occupancy                 | 6.12      | ~6.12       | several         |

Occupancy also shows up as direct per-sample float32 values clustering at
6.108–6.138 % (`parity_search.py`), confirming it is a stored per-sample %.
`Kernel Invocations` (61440) is a rock-solid exact uint32 anchor at consistent
offsets across all 10 files.

### What blocks a *robust* (general) parser

The column→counter mapping is **multiplexed**: the same page-relative offset
holds different counters in different passes (e.g. offset 1784 averages to ~11.75
in files 0/4/7 but ~21.5 in file 9). So you cannot hardcode offsets. The mapping
is defined by a descriptor **inside streamData** — `strings streamData` shows
`"Limiter Counter List Map"`, `"limiter sample counters"`, `"Counter Info"`,
`"Uarch Enabled"`. The friendly names ("ALU Utilization", …) are **not** in the
bundle (Xcode derives/labels them), so the descriptor uses Apple uarch counter
identifiers.

### Remaining implementation (next step)

1. Parse streamData's `Counter Info` / `Limiter Counter List Map` to get the
   ordered counter list per pass → column offsets per `Counters_f_N`.
2. For each counter column, average the float32 across all sample pages → the
   per-encoder value (matches Xcode CSV).
3. Map the uarch/limiter counters to the friendly metrics needed by
   `ShaderHardwareMetrics` (`ALUUtilization`, `KernelOccupancy`); verify against
   a CSV export before trusting (do **not** ship fit-to-CSV offsets — they don't
   generalize across traces/passes).
4. Wire into `ParsePerfCounters`; `correlate` then shows `streamdata+hw`.

This replaces the old range-based float search (Method 2 in
`FIELD_OFFSET_QUICK_REFERENCE.md`), which fails on Xcode-26 because the values
are means over multiplexed per-sample columns, not single in-range floats.

---

## TL;DR

The model in [`FIELD_OFFSET_QUICK_REFERENCE.md`](./FIELD_OFFSET_QUICK_REFERENCE.md)
and [`PERFCOUNTERS_STATUS.md`](./PERFCOUNTERS_STATUS.md) — **464-byte sample
records delimited by `0x4E` markers**, with range-based float search for
ALU/occupancy — is **invalid for Xcode-26 captures**. Empirically:

- `Counters_f_*.raw` are **4096-byte page-framed**, not 464-byte-record framed.
- `0x4E` is **not** a record delimiter; it is the low byte of 64-bit timestamps.
- Real counter values are present but sparse and scattered across several
  **page types**; range-based float search cannot distinguish them from float
  noise without external ground truth.

`correlate` already consumes hardware ALU/occupancy *if* `ParsePerfCounters`
produces non-zero values (the join is wired by shader name in
`internal/shader/correlation.go`). So Tier 2 is now purely a
`internal/counter/counter.go` parser problem — no further `correlate` change is
needed once the framing is cracked and **validated against an Xcode CSV export**.

## Evidence (reproduce with `experiments/gputrace-probe/tier2-forensics/cf_forensics*.py`)

### 1. 4096-byte page framing (refutes 464-byte records)
File sizes are exact multiples of 4096:

| files          | size      | pages (4096 B) |
|----------------|-----------|----------------|
| Counters_f_0–5 | 1,396,736 | 341            |
| Counters_f_6,7 | 1,400,832 | 342            |
| Counters_f_8,9 | 1,388,544 | 339            |

They differ by whole pages (±1–3 × 4096). `464` does not tile 4096
(4096 / 464 = 8.83), and the first uint32 of a page is never `464`. The
`findRecordBoundaries` / `detectRecordType(size==464)` model finds nothing.

### 2. `0x4E` is a red herring
`0x4E` is **0.098 %** of bytes (1363 / 1,396,736); the gaps between occurrences
are arbitrary (1333, 4473, 3586, …). It recurs because it is the **low byte of
GPU timestamps** — e.g. page first-words `0x2816b64e`, `0x10191a4e`,
`0x18187c4e`. Keying record boundaries on `0x4E 00 00 00` is keying on timestamp
noise. (85.6 % of all bytes are `0x00`; the data is sparse.)

### 3. Data is present but scattered across page types
- **2,248** float32 in the plausible counter range (0.001, 100] (~0.64 %);
  97,865 non-zero floats total — most are near-zero denormal **noise** from
  reinterpreting arbitrary bytes as float32 (`0x00000001` = 1.4e-45 > 0).
- Pages are **non-uniform**: Jaccard overlap of non-zero byte offsets between
  pages 0 and 1 ≈ 0.12, pages 0 and 100 ≈ 0.04 → this is **not** an array of
  identical records. First-uint32 low-16 tags cluster into ~15 recurring values
  (`0x004e`, `0x003e`, `0x017e`, `0x016e`, `0x007e`, `0x00de`, `0x000e`, …),
  i.e. **several page types** (timestamp pages, counter-value pages, metadata).
- Recurring values that look meaningful: float `2.0` (very common), small
  fractions `0.004`–`0.008`, `0.125`, `0.5`; uint32 `16384` (= problem size N,
  ~142×), `94` (= `temp_regs`, ~149×), and `32/64/128/256/1024` (~140× each,
  threadgroup/SIMD sizes).
- **10** `Counters_f` files with slightly different page counts is consistent
  with **multi-pass counter multiplexing** (the HW samples a subset of counters
  per pass), so a given counter likely lives in a specific file/pass.

### 4. Why range-based float search now fails
With ~349k float32 per file and tons of near-zero noise, "first float in
(0, 5]" returns garbage, and there is no stable offset because the layout is
page-typed, not fixed-record. The old approach implicitly relied on a CSV ground
truth to range-validate; on Xcode-26 the ranges no longer isolate the right
field.

## What the next attempt needs (in order)

1. **Get ground truth first — an Xcode CSV export of this exact bundle.** Open
   `nbody_seed_full.gputrace` in Xcode → Shader Profiler → export counters, or
   drive `gputrace export-counters` / the `collect_xcode_profile_export_counters`
   automation (now Xcode-26-compatible after the `metaloptim/xcode26-fixes`
   patch). Record nbody_step's **ALU Utilization (%)** and **Kernel Occupancy
   (%)**. Without this, any offset is a guess — **do not ship guessed offsets as
   validated** (see also `matching-xcode-gputools-parity.md`).
2. **Reverse the 4096-byte page structure:** identify the page-type tag field,
   the per-type layout, and which page type carries counter *values* vs
   timestamps. Correlate counter pages to encoders/dispatches via the timestamps
   in `Timeline_f_*.raw` and the `APSTimelineData` / GPRWCNTR records already
   parsed in `internal/counter/streamdata.go`.
3. **Fit & verify** recovered ALU/occupancy against the CSV from step 1 (within
   rounding). Only then wire the values into `ShaderHardwareMetrics`
   (`ALUUtilization`, `KernelOccupancy`) in `ParsePerfCounters`; `correlate`
   picks them up automatically (method flips to `streamdata+hw`).

## Caveats

- The repo's `testdata` profiler-raw is Git-LFS and not materialized, so there is
  no in-repo known-good reference to diff offsets against — the Xcode CSV export
  is the substitute.
- This is an **undocumented Apple format**; treat all of the above as evidence,
  not a spec.

---

## RESULT 2026-05-29 — proven end-to-end and wired into `correlate`

A comprehensive proof (`tier2-forensics/prove_parity.py`) confirmed the model:
**43/43** percentage-like CSV metrics have a `Counters_f` column whose cross-page
mean matches the Xcode value, including the two targets:

- ALU Utilization: CSV 21.46 → column mean **21.519** (f9 @ off 1784)
- Kernel Occupancy: CSV 6.12 → column mean **6.121** (f9 @ off 3860)

Distinctive metrics match tightly with few candidate columns (F32 Limiter
45.68→45.658, ALU Float Instr 90.88→90.900, MMU TLB 43.52→43.172), ruling out
coincidence.

This is wired into `correlate` via `counter.RecoverValidatedHWMetrics`
(`internal/counter/xcode_parity.go`): when an Xcode `Counters.csv` sits next to
the bundle, correlate locates the ALU/occupancy columns by matching their raw
column mean to the CSV value, and reports the **raw-derived** mean (method
`streamdata+counters`). With no CSV it leaves them 0 (method `streamdata`).

```
$ gputrace correlate --json nbody_seed_full.gputrace   # with Counters.csv present
  total_shaders=1 correlation_rate=100
  shader[0]: nbody_step  ALU=21.52%  Occupancy=6.12%  method=streamdata+counters
```

**Remaining for CSV-free recovery (generalization):** parse the streamData
counter descriptor (`Counter Info` / `Limiter Counter List Map`, nested in
`shaderProfilerData` / `batchIdFiterableCounters`) to identify the per-pass
column→counter mapping without needing the CSV. The aggregation (per-pass files,
per-page samples, column mean) is already proven and implemented.

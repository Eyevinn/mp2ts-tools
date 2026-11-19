# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Overview

A collection of Go CLI tools for analyzing and manipulating MPEG-2 Transport Stream (TS) files. Each tool lives under `cmd/mp2ts-<name>/` and shares core logic from the `internal/` package.

## Build and Development Commands

```bash
# Build all tools (outputs to out/)
make build

# Run all tests
go test ./...

# Update golden files after intentional output changes
go test ./internal/... -update

# Lint (requires golangci-lint)
golangci-lint run

# Tidy dependencies
go mod tidy
```

`-version` reports what Go (1.24+) embeds when building a package in a git checkout: the tag or a pseudo-version (with `+dirty` for local changes) and the commit date, read by `internal.Version()` (`buildinfo.go`). Build `./cmd/<tool>` as a package (as the Makefile does), not `main.go` as a file, which gives `(devel)`.

## Architecture

### internal/ — All core logic

There is no `pkg/` directory. All shared code lives in `internal/`.

**Key types:**
- `Options` (`utils.go`) — Configuration struct used by all tools. Each tool's `parseOptions()` sets tool-specific defaults and wires up `flag` parsing.
- `JsonPrinter` (`printer.go`) — Conditional JSON output writer. Methods like `Print(data, show bool)` only emit when `show` is true.
- `AvcPS` / `HevcPS` (`avc.go`, `hevc.go`) — Parameter set state machines that track SPS/PPS/VPS, detect changes via hex comparison, and avoid redundant output.
- `NaluFrameData` (`nalu.go`) — Per-frame output: PID, PTS/DTS, frame type (I/P/B), NAL unit details.
- `StreamStatistics` (`statistics.go`) — Frame rate and GoP duration calculated from timestamp steps.

**Main parse functions in `parser.go`:**
- `ParseAll()` — Full analysis: stream info, parameter sets, NAL units, SEI, statistics
- `ParseInfo()` — Quick PMT/stream metadata only (used by mp2ts-info)
- `ParseSCTE35()` — Ad insertion splice commands (uses gots, not astits)
- `FilterPids()` — Drop specified PIDs, rewrite PAT/PMT

**Other entry points:**
- `ExtractES()` (`extract.go`) — Elementary stream extraction to Annex B format
- `PrepareLoop()` / `Loop.WriteLaps()` (`loop.go`) — The mp2ts-loop pipeline: scan the mapped input once into a `streamIndex` (`loop_scan.go`, with access points, picture numbering, field structure (field pictures, field order, pulldown), seam marking and audio frame headers in `loop_es.go`), choose the loop (`loop_plan.go`), build one lap from the window's own packets (`loop_segment.go`), and write laps with per-lap patching and audio moved across the seam (`loop_lap.go`) to a writer or paced UDP (`loop_send.go`). Packet bytes are read and patched in place (`loop_packet.go`); no TS library is used there. MPEG-2 video is parsed from start codes (`mpeg2video.go`). SCTE-35 cues (`loop_scte35.go`, parsed with gots) are kept only if their whole break is inside the loop, and re-stamped per lap (`pts_adjustment`, event IDs, CRC_32). `cmd/mp2ts-loop/PLAN.md` has the design notes.

**Two TS libraries used for different purposes:**
- `go-astits` — High-level demuxing (PAT/PMT/PES extraction). Used by most analysis tools.
- `gots/v2` — Low-level packet manipulation. Used by SCTE-35 parsing (also in mp2ts-loop) and PID filtering.

**Timestamp handling (`const.go`):**
- 90kHz timescale (`TimeScale = 90000`), 33-bit PTS wrap (`PtsWrap = 1 << 33`)
- `SignedPTSDiff()` / `UnsignedPTSDiff()` handle wraparound

### cmd/ — CLI tools

Every tool follows the same 3-function pattern in `main.go`:
1. `parseOptions()` — Returns `internal.Options` with tool-specific flag defaults
2. A parse function — Calls the appropriate `internal.Parse*()` function
3. `main()` — Calls `internal.ParseParams(parseOptions)` then `internal.Execute(os.Stdout, o, inFile, parseFn)`. Execute handles context/SIGINT, file open/close, and error reporting.

Tools: `mp2ts-info`, `mp2ts-nallister`, `mp2ts-pslister`, `mp2ts-extract`, `mp2ts-timeshift`, `mp2ts-pidfilter`, `mp2ts-loop`.

`mp2ts-loop` is the exception to the 3-function pattern: it has its own `Options`, scan/loop dispatch, and outputs (file/stdout/UDP) in `cmd/mp2ts-loop/main.go` rather than going through `Execute`.

### Testing

Golden file tests in `internal/parser_test.go` (analysis tools) and `internal/loop_scan_test.go` (`mp2ts-loop -scan` reports, golden `testdata/golden_scan_*.json`). Test cases run various `Options` configurations against `.ts` files in `internal/testdata/` and compare output to the golden files. The mp2ts-loop tests (`loop_test.go`, `loop_unit_test.go`) also build synthetic constant-rate streams with a small muxer in `loop_testutil_test.go`.

- Run `go test ./internal/... -update` to regenerate golden files after intentional output changes
- Golden files normalize `\r\n` to `\n` for cross-platform compatibility

## Conventions

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (e.g., `feat:`, `fix:`, `docs:`, `chore:`)
- Manual [CHANGELOG.md](CHANGELOG.md) tracks releases

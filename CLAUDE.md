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

Build injects version info via `-ldflags -X` — the Makefile sets `internal.commitVersion` and `internal.commitDate` at link time from git tags/timestamps.

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

**Two TS libraries used for different purposes:**
- `go-astits` — High-level demuxing (PAT/PMT/PES extraction). Used by most tools.
- `gots/v2` — Low-level packet manipulation. Used by SCTE-35 parsing and PID filtering.

**Timestamp handling (`const.go`):**
- 90kHz timescale (`TimeScale = 90000`), 33-bit PTS wrap (`PtsWrap = 1 << 33`)
- `SignedPTSDiff()` / `UnsignedPTSDiff()` handle wraparound

### cmd/ — CLI tools

Every tool follows the same 3-function pattern in `main.go`:
1. `parseOptions()` — Returns `internal.Options` with tool-specific flag defaults
2. A parse function — Calls the appropriate `internal.Parse*()` function
3. `main()` — Calls `internal.ParseParams(parseOptions)` then `internal.Execute(os.Stdout, o, inFile, parseFn)`. Execute handles context/SIGINT, file open/close, and error reporting.

Tools: `mp2ts-info`, `mp2ts-nallister`, `mp2ts-pslister`, `mp2ts-extract`, `mp2ts-timeshift`, `mp2ts-pidfilter`, `mp2ts-prepare`.

### Testing

Golden file tests in `internal/parser_test.go`. Test cases run various `Options` configurations against `.ts` files in `internal/testdata/` and compare output to `internal/testdata/golden_*.txt`.

- Run `go test ./internal/... -update` to regenerate golden files after intentional output changes
- Golden files normalize `\r\n` to `\n` for cross-platform compatibility

## Conventions

- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) (e.g., `feat:`, `fix:`, `docs:`, `chore:`)
- Manual [CHANGELOG.md](CHANGELOG.md) tracks releases

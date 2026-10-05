# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- New `mp2ts-loop` tool to loop a single-program TS seamlessly and forever
  - Scans for the longest loop bounded by a video random-access point (IDR, CRA for open-GOP HEVC, or an MPEG-2 I picture with sequence header)
  - Constant video frame rate and GOP duration, a loop of whole frame periods (even frame count at 59.94 fps), drift-free audio shifted per wrap by less than one audio frame, and a regenerated linear PCR for constant-rate streams
  - Open-GOP loop points are marked at the seam: HEVC CRA becomes BLA so the seam decodes cleanly; MPEG-2 gets `broken_link`
  - Supports H.264/H.265/MPEG-2 video and AAC/MP2/AC-3 audio; passes through and per-wrap timestamp-shifts other PIDs such as SMPTE-2038 ANC data
  - Output to a file (`-o`, bounded by `-m`) or UDP unicast/multicast (`-a`, paced at exactly the PCR rate); `-scan` prints the loop-point analysis as JSON
  - Refuses to loop streams where an audio or video PES lacks a PTS
  - Keeps SCTE-35 cues whose whole break is inside the loop, re-sent each wrap with shifted time and new event IDs; others are dropped
- mp2ts-info now lists MPEG-2 video PIDs
- mp2ts-nallister now prints per-picture POC (`pic_order_cnt_lsb`) and slice QP (initial SliceQPY) for AVC and HEVC, parsed from the first slice (segment) of each picture

### Changed

- Requires Go 1.24 or later, and uses mp4ff v0.58.0
- `-version` reports the version Go embeds from the git tag and commit (also for `go install`), and names the tool it belongs to; `internal/version.go` and the Makefile `-ldflags` are gone, and `make build` now also builds mp2ts-pidfilter
- mp2ts-pslister now always shows verbose parameter set info (removed `-ps` flag)
- Parameter sets (SPS/PPS/VPS) are only printed when they change, avoiding duplicate output for AVC and HEVC
- AVC PicTiming SEI output now includes all clock timestamp fields (ct_type, counting_type, n_frames, time, time_offset, etc.)

## [0.3.0] - 2025-10-14

### Added

- New `mp2ts-extract` tool to extract elementary video streams from TS files
  - Automatically waits for parameter sets (VPS/SPS/PPS) before extraction
  - Supports both AVC and HEVC streams
  - Can extract specific PID or auto-select first video stream
- Picture type (I, P, B) information for HEVC streams in mp2ts-nallister
- `-waitps` option to mp2ts-nallister to wait for parameter sets before printing NAL units
- SMPTE-2038 data option to mp2ts-tools

## [0.2.1] - 2024-01-25

### Fixed

- Updated Makefile to new project names

## [0.2.0] - 2024-01-23

### Added

- Print image type (I, P, B) for AVC streams
- Calculate GoP duration based on RAI-marker or IDR distance
- Calculate frame-rate based on DTS/PTS and print out in JSON format
- Enable NALU/SEI printing by option
- Print SDT in JSON format
- Support for HEVC PicTiming SEI message
- SEI message data is now also printed as JSON

## Changed

- mp2ts-info and mp2ts-pslister now always print indented output
- mp2ts-nallister -sei option now turns on details.

## [0.1.0] - 2024-01-15

### Added

- initial version of the repo
- ts-info tool

[Unreleased]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.3.0...HEAD
[0.3.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.2.1...v0.3.0
[0.2.1]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.2.0...v0.2.1
[0.2.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.1.0

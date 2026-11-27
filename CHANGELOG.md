# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- mp2ts-loop has a new engine. A lap is the source's own packets with only what a seam needs
  patched: continuity counters continue in the source's own pattern, and the first and last audio
  PES of a lap are cut on frame boundaries instead of being re-packetized one frame per PES
- mp2ts-loop loops variable-rate streams, keeping their source timing, and keeps the PCR within
  40 ms across the seam by turning null packets into PCR-only packets
- mp2ts-loop chooses among loops by picture numbering at recovery points, an even DTS step across
  the seam, whole frame periods, audio from the loop start, and a seam that needs no change of
  output rate, before length; it looks at the PCR stretch between discontinuities a loop lies in
- mp2ts-loop paces UDP output by the output PCR, and sends the PAT and PMT once before the first lap
- mp2ts-loop moves the PES of data streams, such as SMPTE ST 2038 ANC, teletext and subtitles,
  across the seam with the picture of their PTS, as it does audio, so each lap's pictures keep
  their own ANC; a data stream whose PES have no PTS stays where the source sends it, with a
  warning
- mp2ts-loop loops audio and data over the PTS interval of the loop's pictures, from the earliest,
  and a file that lacks a stream's audio or data at an end of the loop leaves that gap in every
  lap, with a warning, instead of filling it from other pictures
- mp2ts-loop `-m` defaults to one lap for file output, `-d` bounds the loop search, and the `-scan`
  report has a new format with the access points, the chosen loop, the lap, continuity errors and
  warnings; a stream that cannot be looped is reported with an error and exit status 1

### Added

- mp2ts-loop loops AVC and HEVC streams without IDR or CRA from recovery point SEI, marked with
  `broken_link_flag` at the seam
- mp2ts-loop reads E-AC-3, and cuts audio whose frames it does not parse (LATM, S302M, AC-4) on PES
  boundaries
- mp2ts-loop `-o -` writes to stdout, `-ttl` sets the multicast TTL, `-q` silences the report, and
  the input may be `-` for stdin
- mp2ts-loop reports the source's continuity errors, warns about those inside the loop, replaces
  packets without sync byte or with an oversized adaptation field by null packets, and warns when
  the PAT lists more than one program (only the first is looped)
- mp2ts-loop refuses content whose video or audio PES do not start at a TS packet, an ISDB-T BTS,
  a video DTS that counts pictures, and a PCR that does not keep time with the video
- mp2ts-loop reads how interlaced pictures are shown: MPEG-2 field pictures, H.264 PAFF and H.265
  field coding are paired into frames and never start or end a loop on a second field, the field
  order and pulldown (`top_field_first`, `repeat_first_field`, H.264/H.265 `pic_struct`) must match
  at both ends of a loop, so 3:2 pulldown loops hold whole cadence cycles, and both fields of an
  H.265 frame coded as two CRA become BLA at the seam
- mp2ts-loop detects the frame rate from how long pictures are shown (25 for 1080i25 field
  pictures, 29.97 for 3:2 pulldown), and its `-scan` report gives a loop's frame periods and coded
  pictures, and the video's field pictures, field order and pulldown pictures

### Fixed

- mp2ts-nallister, mp2ts-pslister and mp2ts-loop no longer panic on a corrupt HEVC slice header,
  with mp4ff v0.63.0. mp2ts-pslister shows the SPS fields that version adds

## [0.4.0] - 2026-10-09

### Added

- New `mp2ts-timeshift` tool to shift all PTS/DTS/PCR values in a TS by an offset, e.g. to test timestamp wrap-around
- New `mp2ts-loop` tool to loop a single-program TS seamlessly and forever
  - Scans for the longest loop bounded by a video random-access point (IDR, CRA for open-GOP HEVC, or an MPEG-2 I picture with sequence header)
  - Constant video frame rate and GOP duration, a loop of whole frame periods (even frame count at 59.94 fps), drift-free audio shifted per wrap by less than one audio frame, and a regenerated linear PCR for constant-rate streams
  - Open-GOP loop points are marked at the seam: HEVC CRA becomes BLA so the seam decodes cleanly; MPEG-2 gets `broken_link`
  - Supports H.264/H.265/MPEG-2 video and AAC/MP2/AC-3 audio; passes through and per-wrap timestamp-shifts other PIDs such as SMPTE-2038 ANC data
  - Output to a file (`-o`, bounded by `-m`) or UDP unicast/multicast (`-a`, paced at exactly the PCR rate); `-scan` prints the loop-point analysis as JSON
  - Refuses to loop streams where an audio or video PES lacks a PTS
  - Keeps every packet's source timing; audio multiplexed outside the loop is moved across the loop boundary
  - Refuses variable-rate streams, detected from the PCR; a loop never spans a signalled discontinuity
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

[Unreleased]: https://github.com/Eyevinn/mp2ts-tools/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/Eyevinn/mp2ts-tools/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/Eyevinn/mp2ts-tools/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/Eyevinn/mp2ts-tools/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/Eyevinn/mp2ts-tools/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.1.0

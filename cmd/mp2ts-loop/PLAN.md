# mp2ts-loop — design & implementation plan

`mp2ts-loop` takes a single-program MPEG-2 TS file and **loops it seamlessly forever**:

- Video kept at **constant frame rate** and **constant GOP duration**.
- Loop seam is at an **IDR** picture, or a **CRA** when the stream has no IDR (for MPEG-2
  video: an I picture after a sequence header, closed GOP preferred).
- Audio matched to the **average** loop duration with **no accumulated drift** (a small
  sub-frame shift per loop is allowed).
- **Perfect PCR** for a constant-rate TS.
- Handles audio that is **not PES-aligned** (may split an audio PES packet).
- An initial **SCAN** phase finds the **longest** possible loop given the GOP structure
  and the audio displacement of the capture.

It is a single tool: `SCAN → SELECT → PREPARE (in memory) → LOOP/SEND`.

## Decisions (locked)

- Fresh implementation, sourcing logic from the external reference `../tsloop`.
- Single tool `cmd/mp2ts-loop` (scan + loop), with `-scan` (report only) and
  `-write-prepared` (dump the one-loop segment) modes for debugging.
- AAC first, including non-PES-aligned audio + PES splitting, behind a codec-neutral
  `AudioFramer` interface so AC-3/MP2 slot in later.
- Shared engine in `internal/` (no `pkg/`); consolidate PCR/PTS math into `internal/const.go`.

## Libraries

**gots/v2 for the entire pipeline (scan + prepare + loop).** astits stays only in the other
analysis tools; `mp2ts-loop` does not use it.

Why gots end-to-end rather than astits-for-scan:
- Scan and emit must share one **packet-indexed, CC-aware** representation. The emit passes
  read raw 188-byte packets and rewrite in place at known packet numbers; the scan must give
  them exact PES-start packet numbers, the per-packet continuity-counter history (for
  `CalcStep`/`NewCC` loss replay and a gapless seam), and per-packet PCR + total packet count
  (for constant-rate verification and NTP/PCR pacing). astits hides all three (one parsed PES
  + a single `FirstPacket`, no running packet index, no per-packet CC) — an astits scan would
  still need a second gots pass, then reconciliation between two demuxers.
- gots gives the per-picture detail we need: `packet.ContinuityCounter`,
  `adaptationfield.PCR`/`RandomAccess`, exact `StartPktNr`, and ElStream-style PES reassembly
  with the `AlignOffset` carry that non-PES-aligned audio relies on.
- Aligns with the reference (`pkg/ts` is gots-based) → faithful, lower-risk port; the dead v2
  branches (`fix-pidfilter`) are a v2-correct oracle.

NAL parsing (IDR/CRA/field/recovery SEI) uses **mp4ff** on the PES payload — independent of
the TS library. PMT/descriptor parsing uses gots `psi` (already used by `FilterPids`).
**No astits muxer** for output (it owns CC and re-spaces packets → would break the seam/PCR).

**Logging: `log/slog`** (not logrus, not the bare `log` package). Engine leaf functions stay
logging-free and surface conditions via counters/returns (e.g. `ElStream.PktLosses`); slog is
used for higher-level scan/prepare/loop warnings.

The engine ported into `internal/` (from the reference / v2 branches): `pesdata` (PES
reassembly + header parse), `elstream` (per-PID PES assembly with `AlignOffset`),
`continuitycounter`, a gots `InitTS`/packet-walk reader, `video` (IDR/CRA/field/recovery
detection), and `RewritePCR`.

## Extensibility requirements (designed in from day 1)

These three reshape the core abstractions even where v1 doesn't fully implement them:

### 1. Fractional frame rates
Per-frame tick step is not constant for 59.94 (1501.5), 23.976 (3753.75), etc.; it dithers
±1 tick around an ideal **rational** grid `tick(n) = round(n * 90000 * den / num)`.
- Constant-rate check = "all steps within a bounded jitter of the ideal rational grid",
  NOT `min == max`.
- The loop length in ticks must be an **integer**; choose `F` frames where
  `F * 90000 * den / num` is integer (e.g. 59.94 ⇒ `F` even; 23.976 ⇒ `F` multiple of 4).
- Frame period is represented as a rational `{num, den}`, never a single int.

### 2. Interlace / field-coded video (esp. HEVC, one field per PES)
For field-coded HEVC each **field** is its own access unit / PES — so `1 PES = 1 field`.
- Per-picture record carries **field parity** (top / bottom / frame / unknown), derived from
  pic_timing SEI `pic_struct`, SPS VUI `field_seq_flag`, or AVC slice `field_pic_flag`.
- A frame = a (top, bottom) field pair. The loop seam must be at a **frame boundary with the
  correct field parity** (match the stream's opening parity), and the loop length must be an
  integer number of frames (even number of fields).
- Frame-rate / GOP measurement runs on the field cadence and folds field pairs into frames.

### 3. Gradual Decoder Refresh (GDR) — FUTURE, but keep the seam open
GDR streams have **no RAP/IRAP** pictures; only VPS/SPS/PPS (and recovery_point SEI) sent
periodically. So **loop-point eligibility is a pluggable policy**, never hardwired to IRAP:
- v1 policies: `IDR`, then `CRA` when a stream has no IDR.
- future policy: `RecoveryPoint`/GDR — any clean AU boundary, with parameter sets
  guaranteed/re-inserted at the seam.
- The per-picture scan record therefore carries: NAL-type set, `isIDR`/`isCRA`/`isRAP`,
  `hasRecoverySEI`, field parity, and whether VPS/SPS/PPS are present or recently seen — so
  PREPARE can re-insert parameter sets at the chosen seam (needed for CRA today, GDR later).

## MPEG-2 video (H.262)

Parsed from start codes (`internal/mpeg2video.go`), no NAL layer. A loop point is an I picture
preceded by a sequence header (the 13818-1 random-access definition). Its GOP maps onto the
existing classes, so the selection ladder is shared:
- **closed** (`IsIDR`, label `I-closed`): `closed_gop=1`, or `closed_gop=0` but the next coded
  picture is not a B-picture (no leading pictures; promoted after the next PES is scanned).
- **open** (`IsCRA`, label `I-open`): leading B-pictures predict from the previous GOP. At the
  seam that is the previous wrap's tail, so they decode without errors but with wrong content.
  The loop-point GOP gets `broken_link=1` (the 13818-2 edit marker, the analog of CRA→BLA), and
  the plan warns. ffmpeg ignores `broken_link`; there is no pixel-perfect open-GOP seam without
  re-encoding.
- A class needs two points: encoders that close only their first GOP fall through to open.

Validated on ffmpeg encodes (576p25/1080i25 open GOP + MP2, 720p29.97 closed GOP + AC-3, 720p50
without B-frames + AAC): CC 0, exact PTS grid across seams, ffmpeg decode clean. Interlaced
frame pictures (`+ilme+ildct`, constant top_field_first) loop like progressive; field pictures,
repeat_first_field / pulldown, and parity continuity belong to the interlace work
(extensibility requirement 2).

## Core types / interfaces (the extensibility seams)

```
// one coded picture OR one field
type PicRecord struct {
    PTS, DTS    int64
    PktNr       int          // first TS packet of the AU
    RAI         bool         // adaptation-field random_access_indicator
    NalTypes    []uint32
    IsIDR       bool
    IsCRA       bool
    IsRAP       bool         // any IRAP (IDR/CRA/BLA)
    HasRecovery bool         // recovery_point SEI (for GDR)
    Field       FieldParity  // Top | Bottom | Frame | Unknown
    PSPresent   bool         // VPS/SPS/PPS carried in this AU
}

// pluggable loop-seam eligibility (IDR / CRA / future GDR)
type LoopPointPolicy interface {
    Eligible(pics []PicRecord, i int) bool
    Name() string
}

// codec-neutral audio (AAC now; AC-3/MP2 later)
type AudioFramer interface {
    FrameDurTicks() int64               // 90 kHz ticks per audio frame
    Split(pes *PESData) (*AudioPES, error) // frames + Offset/Missing carry
    Silence(pid uint16, pts int64) []byte  // one silent-frame TS packet
    // trim/crop/extend modifiers built on PacketModifier
}
```

## Milestones

- **M0 — Scaffold & math.** `cmd/mp2ts-loop/main.go` (self-contained, like timeshift), flags,
  Makefile entry, `-version`, `-h`; extend `internal/const.go` with PCR helpers
  (`AddPCR`, `SignedPCRDiff`, `UnsignedPCRDiff`).
- **M1 — SCAN: video.** Per-picture `PicRecord` stream from refactored avc/hevc parsing;
  add **HEVC CRA** + BLA detection and field parity; rational frame-period detection with
  bounded dither; `LoopPointPolicy` (IDR, CRA-if-no-IDR). JSON scan report.
- **M2 — SELECT: longest loop.** Largest IDR/CRA interval with constant GOP, integer-tick
  length (rational), frame-boundary/parity, and an audio fit for every ES. `-d` = optional cap.
- **M3 — Audio (AAC).** `SplitADTSPES` with Offset/Missing carry (non-PES-aligned), gap
  quantization + bounded `accPTSShift` anti-drift, behind `AudioFramer`.
- **M4 — PREPARE (in-memory).** Build one loopable segment: video copy `[vStart..vEnd)`,
  PAT/PMT + parameter sets at the seam, jitter snap; audio trim/crop/extend + silence; CC
  regen; handle packets that already carry an adaptation field.
- **M5 — LOOP / SEND.** Per-wrap PTS/DTS/PCR offset, audio `deltaPTS` carry, seamless CC,
  SCTE-35 PTS shift; file + UDP (7 TS/datagram, NTP-locked pacing).
- **M6 — Robustness.** CRA open-GOP leading-picture (RASL) handling; multiple-SPS tolerance;
  A/V start displacement; incomplete SCTE pairs.
- **M7 — Tests & docs.** Golden scan-report tests; functional multi-wrap test on a real long
  capture; README + CHANGELOG + CLAUDE.md.

## Known limits / v1 scope

- PCR "perfect" = replay identical prepared segment + constant offset (preserves slope);
  bitrate-model PCR regeneration is a stretch goal.
- 44.1 kHz AAC (non-integer ticks): warn, not supported in v1.
- Single program, single video track (documented).
- GDR loop policy is stubbed (interface present, not implemented).

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
  **Done (2026-10-02):** `selectLoop` prefers the longest exact loop (rate from `-fps` or the
  DTS step) and warns if none exists; `LoopPlan.Frames` reports `F`.
- Frame period is represented as a rational `{num, den}`, never a single int.
- Audio: the per-wrap shift (`audioWrapState.deltaPTS`) stays in `[0, frameDur)`, so it is always
  less than one audio frame and never accumulates; the audio cadence is continuous across seams.
- Validated 2026-10-02 (29.97/59.94/23.976 × AAC/MP2/AC-3, 200 wraps): audio shift < 1 frame for
  every wrap; video PTS/DTS keep the source's exact rational-grid dither across seams. One caveat
  comes from the source: an encoder's first GOP can carry a start-up DTS quirk (x264: 1 tick),
  which a loop starting there replays every wrap. Real captures do not start at the encoder start.

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

## Interlace — plan (next, after MPEG-2)

Expands extensibility requirement 2. The scan is field-unaware today: every PES is one
`PicRecord` with `Field` = frame (MPEG-2 fills it from `picture_structure` but nothing uses it),
and the report's `field` is hard-coded `"frame"`.

**What already loops cleanly (validated 2026-09-30, ffmpeg decode clean, field order continuous
across seams):**
- Frame-coded interlace with a constant field order: MPEG-2 `+ilme+ildct` 1080i25, AVC MBAFF
  (x264 `tff=1`). These are frame pictures, so they loop like progressive.
- HEVC field coding from x265 `interlace=tff` (one field per PES, pic_timing `pic_struct` 1/2),
  but only by luck: x265 puts every IDR on a top field every 50 fields. The report calls it
  50 fps frames. ffmpeg's HEVC interlace support is weak (FFmpeg PR #23616), so this is not a
  reliable check.

**Shared infrastructure (all codecs):**
1. Every audio and video PES must carry a PTS (decided 2026-10-02; done). The scan counts PES
   without one (`pesWithoutPTS` in the report) and refuses to loop, so field-per-PES streams
   that omit the second field's PTS are out of scope.
2. Per-picture field parity (top/bottom/frame) plus pairing: one PES may hold a field pair (AVC
   PAFF, MPEG-2 field pictures), or one field (HEVC, some PAFF muxes).
3. Fold fields into frames for frame rate, GOP, and report (`field`: progressive / interlaced
   frame / field-coded, and the first-field parity).
4. Loop-point eligibility: the first field of a frame, with the stream's first-field parity.
   Loop length is a whole number of frames (even field count), so field order continues across
   the seam.
5. Validation: validate.py per-field parity across seams, plus
   `ffprobe -show_entries frame=top_field_first,interlaced_frame`.

**MPEG-2** (easiest; mostly done): frame pictures work. Remaining: field pictures (pair in one
PES or split over two), `top_field_first` continuity, and `repeat_first_field` pulldown (below).
The ffmpeg encoder cannot make field pictures, so field-picture MPEG-2 needs a broadcast capture.

**AVC — MBAFF vs PAFF:**
- MBAFF (`mb_adaptive_frame_field_flag`, `field_pic_flag=0`): frame pictures, loops like
  progressive. Only field order continuity (pic_timing `pic_struct` 3/4) needs checking. Test
  content: x264 `tff=1` / `bff=1`.
- PAFF (`field_pic_flag=1`, `bottom_field_flag`): each field is its own access unit and needs a
  slice-header parse with the active SPS (`frame_mbs_only_flag`, `log2_max_frame_num`). mp4ff
  and the nallister POC/QP code already do this. An IDR is the first field only; the second
  field is non-IDR. Streams can switch between PAFF field pairs and MBAFF frames per picture, so
  classify per AU. No local encoder makes PAFF (x264 does MBAFF only): needs a broadcast capture.
- Related, common in broadcast interlaced AVC: open GOP without IDRs, where loop points are
  non-IDR I pictures with a recovery_point SEI (`recovery_frame_cnt=0`). The seam marker is the
  SEI's `broken_link_flag` (the analog of MPEG-2 `broken_link` and HEVC CRA→BLA).

**HEVC — field coding** (`field_seq_flag=1`, most complex; **on hold until example files are
provided**, since ffmpeg/x265 cannot be trusted for HEVC interlace): no interlace coding tools; each field
is a picture/AU (usually one PES). Parity comes from the pic_timing SEI `pic_struct` (1/2, or 9–12
paired with the previous/next field). It needs VUI `frame_field_info_present_flag`, and mp4ff
already parses `FrameFieldInfo`. Work items:
- IRAP on the first field; reject or skip loop points on the wrong parity.
- Open GOP: if both fields of the loop-point frame are CRA, the second field's CRA is in the next
  AU. `rewriteSeamCRAtoBLA` only patches the first AU, so it must patch both. RASL leading fields
  are dropped at the BLA as before.
- Frame-coded interlaced HEVC (`field_seq_flag=0`, `pic_struct` 3/4) loops like progressive.
- Test content: example files from the user (pending). x265 `interlace=tff` + `separatefields`
  only for quick experiments (odd keyint in fields forces bottom-field IRAPs). The GDR caps
  (`cap_*_i25`) also need the GDR policy.

**Pulldown / repeat-field** (MPEG-2 `repeat_first_field`, AVC/HEVC `pic_struct` 5–8): coded
frame cadence ≠ display cadence, so DTS steps alternate (e.g. 3003/4504) and the constant-rate
check fails. The loop must hold whole pulldown cycles and continue the TFF/RFF pattern.
Initially detect and warn/refuse.

**Order:** shared infrastructure (2–4) → MBAFF + MPEG-2 frame-picture parity checks (testable
now) → PAFF and MPEG-2 field pictures (need captures) → HEVC field coding (when example files
arrive) → pulldown.

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
  SCTE-35 PTS shift (done 2026-10-05: `pts_adjustment` + event IDs per wrap, `loop_scte35.go`);
  file + UDP (7 TS/datagram, NTP-locked pacing).
- **M6 — Robustness.** CRA open-GOP leading-picture (RASL) handling; multiple-SPS tolerance;
  A/V start displacement; incomplete SCTE pairs (done: a cue is kept only if its whole break,
  CUE-OUT to CUE-IN or OUT + duration, is inside the loop; pre-loop cues for such breaks move to
  the loop start).
- **M7 — Tests & docs.** Golden scan-report tests; functional multi-wrap test on a real long
  capture; README + CHANGELOG + CLAUDE.md.

## Transport timing (2026-10-05)

The segment keeps one slot per source packet of the video window, so in a constant-rate stream
every packet keeps its source arrival time (the regenerated linear PCR equals the source's).
Stuffing and the window's original audio slots are free; each kept audio PES goes into the first
free slots at or after its target (`placeAudio`). Audio is selected by PTS but multiplexed with an
offset to the video (usually lagging, by the difference in buffer delays), so audio from after the
window moves one loop duration (`idealPackets`) back to the start, and belongs to the previous
wrap (rot -1); audio from before the window moves to the end and belongs to the next wrap (rot +1).
The per-wrap audio shift is computed from the segment's frame list (`audioWrapState.delta`), so
it does not depend on the send order. Wrap 0 sends no rot -1 audio, the last wrap of a bounded run
no rot +1 audio. The window's transmission time differs from the loop duration by the change in
decoder buffer delay between the two loop points (`stuffingDelta`); free slots are removed from the
end of the window, or nulls added before the seam, so packets just before the seam move by that
much. Measured per PES on captures: 0 ms change for most of the wrap, up to 80-113 ms at the seam.

**VBR sources are refused** (decided 2026-10-05): `constantRate: false` from the PCR (intervals
between PCRs within 0.5%; each stretch between signalled discontinuities on its own), e.g. football
and three of the four bundled fixtures (0.24-1.1 s from a constant rate). A loop never spans a
packet with discontinuity_indicator (PCR, audio or video PID). VBR support would keep each packet's
source PCR timing, compress only the seam, and pace UDP by the PCR.

Open:
- Choose loop points with matching buffer level (small `stuffingDelta`) to shrink the seam shift.
- An encoder's first GOP may have no free slots for moved audio (x264/ffmpeg send no audio before
  ~0.1 s); the moved audio then arrives up to ~90 ms later than in the source (still ahead).

## Known limits / v1 scope

- PCR "perfect" = replay identical prepared segment + constant offset (preserves slope);
  bitrate-model PCR regeneration is a stretch goal.
- 44.1 kHz AAC (non-integer ticks): warn, not supported in v1.
- Single program, single video track (documented).
- GDR loop policy is stubbed (interface present, not implemented).

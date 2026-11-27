# mp2ts-loop — design notes

`mp2ts-loop` makes a perfect endless loop of a single-program MPEG-2 TS, for test sources that run
for hours: a decoder or a TR 101 290 analyser should not be able to tell the seam from the rest of
the stream. It does no remux. A lap is the source's own packets from one video access point to a
later one, and only the fields that have to change are patched.

## History

- 2026-10: first engine (gots/ElStream two-pass reader, audio re-packetized one frame per PES,
  regenerated continuity counters, constant-rate input only).
- 2026-11-18: a new engine, modelled on the first: one scan of the mapped input, a lap built from
  the window's own packets and patched in place, audio cut on frames at the seam. Checked on 23
  inputs: the bundled fixtures, 13 ffmpeg encodes across codecs, rates and GOP structures, and
  edge cases (PTS wrap, two programs, a lost packet, corrupt packets).
- 2026-11-19: interlace (below): field pictures, field order and pulldown.
- 2026-11-26: every stream loops the video's PTS interval, from the earliest PTS of the loop's
  pictures; data PES (SMPTE ST 2038 ANC, teletext, subtitles) move across the seam with the
  picture of their PTS, as audio does. An encoder that sends ANC three frames ahead of its
  picture otherwise gave each lap's first pictures the ANC of pictures after the loop's end.

## Pipeline

`internal/loop.go` ties it together: `PrepareLoop` reads the input (mapped on unix), then

1. **Scan** (`loop_scan.go`): one pass over the packets. The first program of the PAT and its PMT
   (a descriptor that does not parse is skipped, not the PMT), the video (first H.262/H.264/H.265
   PID), audio (MPEG audio, ADTS, AC-3/E-AC-3 cut on frames; LATM, S302M, AC-4 on PES), data
   (any other PES stream), SCTE-35 and pass-through PIDs (sections, a second video). Per picture:
   PTS/DTS (unwrapped), access point class, parameter sets, picture numbering and how it is shown
   (`loop_es.go`, see Interlace). Per audio PES: its frames and where its bytes lie. A PES whose
   frames cannot be read, as after a lost packet, is one frame that lasts until the next PES, so
   no lap ends inside it; when that is most PES (frames that straddle PES), the whole stream is
   cut on PES boundaries. Per data PES:
   its PTS and where its bytes lie, unless a PES of the stream has no PTS, a header that cannot be
   read or a PTS earlier than the one before, which keeps the stream where it is sent. Also PCR
   samples, discontinuities, null packet counts, payload units per PID, continuity errors (a
   duplicate packet is allowed, and its payload is read once), and PES that do not start a
   packet's payload (refused on video and audio, warned on others). A BTS (204-byte packets) is
   refused.
2. **Choose** (`loop_plan.go`): the PCR as stretches between discontinuities, each constant-rate
   when within 1 ms of a straight line. The input is refused when the median DTS step gives more
   than 300 pictures a second, or the PCR and video PTS part by more than 1 s plus a tenth of a
   stretch. Access points of the cleanest class with two (closed GOP, open GOP, recovery point),
   with those of a cleaner class among them, never a second field, are paired longest first. A
   loop is of the class of its first picture, where every lap starts: an IDR among recovery
   points starts a closed-GOP loop, whatever follows the loop. Ranking: picture numbering kept
   (always by a loop that starts on an IDR or IRAP, by one that starts on a recovery point when
   the picture after the loop has its numbers), DTS step even across the seam (within half a
   picture), the same field order and pulldown phase at
   both ends, whole frame periods, audio from the loop start, a seam needing no rate change,
   longest. A pair is skipped when its window crosses a discontinuity, is sent far off the
   time it lasts, shows a picture after its end, or would jump the DTS by more than 1 s. At most
   16 million pairs are looked at. Every stream loops the PTS interval of the loop's duration
   from the earliest PTS of the window's pictures (before the access point's when leading
   pictures are shown first): audio from the frame nearest its start to its end, and data PES
   whose PTS lie from half a frame before it to half a frame before the next lap's. Audio the file
   lacks at the loop's start (more than half a frame) is a gap at the start of every lap, not made
   up with audio from after the loop's end; data PES of the loop's first or last pictures that the
   file lacks are counted and warned about. A data stream whose window PES are timed more than a
   loop from the video, or whose loop PES are sent more than a loop away, stays where it is sent.
3. **Build a lap** (`loop_segment.go`): the window's packets with corrupt ones nulled,
   discontinuity flags cleared, PES and sections cut by the edges stripped (a PCR stays as an
   adaptation field), audio and placed data packets taken out (their PCR kept), and for constant-rate input null
   packets removed or added at the end so a lap lasts exactly its duration. Send times are even for
   constant rate and the source's for variable rate. Null packets around the seam become PCR-only
   packets when the PCR would be more than 40 ms apart. The first picture is marked as an edit:
   CRA → BLA_W_LP, MPEG-2 `broken_link`, recovery point SEI `broken_link_flag`; so is its second
   field when that is an access point too (two CRA fields). The audio a lap
   plays is prepared per PES, each packet due where the source sends it relative to the window;
   the PES holding the first frame loses the frames before it. Each data PES of the loop is
   prepared whole the same way.
4. **Write laps** (`loop_lap.go`): per lap, the audio controller picks how many frames end the lap
   so the next one continues without a timestamp jump (never earlier than the video, less than a
   frame later, no drift; a lap with a gap at its start ends at the loop's end all the same).
   Audio and data sent after the window go out at the start of the next lap, those sent before it
   at the end of the previous one, into free packets, or inserted (a constant-rate lap then drops
   a free null packet). Continuity counters keep the source pattern with one offset per PID and
   lap (audio and placed data are counted afresh); PCR, OPCR and PES timestamps advance by the loop
   duration. A bounded run ends with the audio and data the next lap would have carried. The
   output is made in chunks of 3584 packets (512 datagrams) by a goroutine that stays two chunks ahead
   of the outputs (`WriteLaps` in `loop.go`), so a paced output does not wait at a seam while the
   next lap's audio and data are placed. When the run is stopped, a paced output stops at once
   and a file at the end of the lap.
5. **Outputs** (`loop_send.go`): a writer (file or stdout) as fast as it goes, and UDP, 7 packets
   per datagram, each sent when its last packet is due by the output PCR on the monotonic clock.

## Kept from the first engine

- **SCTE-35** (`loop_scte35.go`): a cue is kept only when its whole break, CUE-OUT to CUE-IN or
  CUE-OUT plus its duration, lies inside the loop; others are dropped, freeing their packets. A cue
  sent before the window for such a break is injected at the start of each lap. The counters of a
  PID whose packets changed are renumbered. Each lap re-stamps the kept sections, including ones
  that span packets: `pts_adjustment` advances with the video, and every event ID by one more than
  the span of the kept IDs, so each lap's events are new.
- **MPEG-2 field pictures**: an open-GOP I picture alone in its PES is followed by its other field
  before the picture that tells whether it has leading B pictures.
- **The JSON report** of `-scan`, now with the access point classes, the loop, the lap, continuity
  errors and warnings.

## Interlace

Each picture has a field structure (`pictureInfo` in `loop_es.go`):

- **Field pictures**, alone in their PES: MPEG-2 `picture_structure`, H.264 `field_pic_flag` and
  `bottom_field_flag`, H.265 `pic_struct` 1, 2, 9-12 (with `field_seq_flag`). The H.264 and H.265
  slice header is read only as far as the field flags and the PPS ID, so a slice mp4ff cannot
  parse still tells. A PES that holds both fields (an access unit delimiter or a slice that starts
  a picture after the first) is a frame that shows its first field first.
- **Pairing** (`pairFields`): a field completes an unpaired field of the other parity just before
  it in decode order when it is shown after it, within 1/24 s, shares its H.264 `frame_num` or
  MPEG-2 `temporal_reference`, and is not an H.264 IDR or an MPEG-2 field after a sequence header,
  which are always first fields. A field lost from the source leaves its partner unpaired instead
  of putting the pairing out of step; in the captures looked at, H.265 encoders send the fields
  of a frame one after the other in decode order.
- **Frames**: the field shown first (MPEG-2 `top_field_first`, `pic_struct` 3-6) and how many field
  periods the frame is shown (2; 3 with `repeat_first_field` or `pic_struct` 5-6; 4 or 6 for an
  MPEG-2 progressive sequence's `repeat_first_field` or `pic_struct` 7-8). H.264 `pic_struct`
  follows the CPB and DPB delays in pic_timing when the SPS has HRD parameters, so the SPS is
  parsed with its whole VUI (and without it when that fails).

The loop choice uses it three ways:

- A second field is never a loop point: the lap would start or end inside a frame.
- The **cadence** of the loop's first picture and of the picture after its end (the field shown
  first, and how long the frame or field pair is shown) must match, or the loop ranks lower and
  is warned about: then the fields keep alternating across the seam, and a 3:2 pulldown loop holds
  whole cadence cycles. With 13-frame GOPs, only every fourth GOP is in phase.
- The **frame rate** is detected from how long pictures are shown: the median, in display order,
  of the PTS step to the next picture per two field periods the picture lasts (falling back to the
  median DTS step). 1080i25 field pictures give 25, 3:2 pulldown 29.97. Whole frame periods are
  counted in field periods, or in coded pictures for a rate given as the field rate (field
  pictures) or the film rate (pulldown).

Checked on H.265 field-coded captures (VA-API, broadcast, an encoder with a lost field), H.264
with both fields in a PES and MBAFF, ffmpeg MPEG-2 and H.264 frame-coded interlace, and an MPEG-2
3:2 soft-telecine stream (ffmpeg at 23.976 fps, flagged like DGPulldown): loops start on first
fields, field steps stay even across seams, and the pulldown pattern runs on unbroken.

Open:

- A picture before the stream's first parameter sets has no field structure; it is counted as a
  frame. Such pictures come before the first access point, so no loop holds them.
- H.265 pairing does not use `pic_struct` 9-12, which says which field pairs with which.
- Pulldown that is not signalled, with only the timestamps stepping unevenly, is not recognised;
  the frame rate is then unknown, and `-fps` gives it.
- The report's `fieldOrder` is `mixed` for pulldown, whose field order changes by design.

## Known limits

- Only the first program is looped; others are cut at its loop points and break at every seam.
- A second video stream in the program is not moved by PTS: it stays where the source sends it
  and breaks at every seam.
- A recovery point SEI is read with the rest of its SEI NAL unit; a message there that does not
  parse hides it. `broken_link_flag` is not set when an emulation prevention byte comes before it.
- Memory: the input is mapped (read when it is a pipe), and the laps are made from one copy of
  the loop's window, plus per-packet scan tables (about 4 bytes a packet). `-d` bounds the loop.

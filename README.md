![Test](https://github.com/Eyevinn/mp2ts-tools/workflows/Go/badge.svg)
[![golangci-lint](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml)
[![GoDoc](https://godoc.org/github.com/Eyevinn/mp2ts-tools?status.svg)](http://godoc.org/github.com/Eyevinn/mp2ts-tools)
[![license](https://img.shields.io/github/license/Eyevinn/mp2ts-tools.svg)](https://github.com/Eyevinn/mp2ts-tools/blob/master/LICENSE)

# mp2ts-tools - A collection of tools for MPEG-2 TS

MPEG-2 Transport Stream is a very wide spread format for transporting media.

This repo provides some tools to facilitate analysis and extraction of
data from MPEG-2 TS streams.

## Tools

### mp2ts-info

`mp2ts-info` parses a TS file or stream on stdin and prints information about the video streams in JSON format. Use this for quick stream analysis and metadata extraction.

**Example:**
```sh
mp2ts-info video.ts
```

### mp2ts-nallister

`mp2ts-nallister` shows detailed information about NAL units including:
- PTS/DTS timestamps
- Picture types (I, P, B frames) for both AVC and HEVC
- PicTiming SEI messages with detailed clock timestamp fields
- RAI (Random Access Indicator) markers
- SMPTE-2038 ancillary data

**Options:**
- `-waitps` - Wait for parameter sets (SPS/PPS) before printing NAL units
- `-sei` - Print detailed SEI message information
- `-smpte2038` - Print SMPTE-2038 ancillary data details
- `-max N` - Limit output to N pictures

**Example:**
```sh
mp2ts-nallister -waitps -max 10 video.ts
```

### mp2ts-pslister

`mp2ts-pslister` shows verbose information about parameter sets (SPS, PPS, and VPS for HEVC) in a TS file. Only prints parameter sets when they change, avoiding duplicate output for unchanged sets. Useful for debugging video codec configurations.

**Example:**
```sh
mp2ts-pslister video.ts
```

### mp2ts-extract

`mp2ts-extract` extracts elementary video streams (PES payloads) from TS files to raw Annex B byte stream format. By default, it waits for parameter sets (VPS/SPS/PPS) before starting extraction to ensure a clean, decodable stream.

**Features:**
- Supports both AVC (H.264) and HEVC (H.265) streams
- Auto-selects first video PID or extract specific PID
- Outputs Annex B byte stream format
- Waits for parameter sets by default

**Options:**
- `-output <file>` - Output file path (required, use `-` for stdout)
- `-pid N` - PID to extract (0 = auto-select first video PID)
- `-waitps` - Wait for parameter sets before extraction (default: true)

**Examples:**
```sh
# Extract first video stream to file
mp2ts-extract -output video.264 input.ts

# Extract specific PID
mp2ts-extract -pid 512 -output video.hevc input.ts

# Output to stdout
mp2ts-extract -output - input.ts > video.264
```

### mp2ts-timeshift

`mp2ts-timeshift` shifts all PTS/DTS/PCR_base values in a transport stream by a specified offset. The main use-case is to generate TS files with timestamp wrap-around for testing purposes.

**Options:**
- `-offset N` - Timestamp offset in 90kHz units (can be negative)
- `-output <file>` - Output file path (default: `-` for stdout)

**Examples:**
```sh
# Shift by 2^33 to cause wrap-around
mp2ts-timeshift -offset 8589934592 -output output.ts input.ts

# Shift back by 100 seconds
mp2ts-timeshift -offset -9000000 input.ts > output.ts
```

### mp2ts-loop

`mp2ts-loop` makes a perfect endless loop of a single-program transport stream, for test
sources that run for hours: a decoder or a TR 101 290 analyser should not be able to tell
the seam from the rest of the stream. It does no remux. A lap is the source's own packets
from one video access point to a later one, and only the fields that must change are
patched.

The loop starts and ends at video access points of the cleanest class there are two of:
an IDR (or an MPEG-2 I picture with a sequence header in a closed GOP), an HEVC CRA or
open MPEG-2 GOP, or an AVC/HEVC recovery point. Among all pairs it prefers a loop that
keeps the picture numbering at a recovery point, a DTS that steps across the seam as
elsewhere, fields that keep alternating across the seam, a whole number of frame periods
(an even frame count at 59.94 fps, a multiple of four at 23.976), audio from the loop
start, a seam that needs no change of output rate, and then the longest loop. Content whose timestamps do not fit together (a DTS that counts
pictures, a PCR that does not keep time with the video) is refused with the numbers that
show why.

Each lap continues the continuity counters in the source's own pattern and advances PCR
and PES timestamps by the loop duration. A constant-rate stream gets an exactly linear PCR,
with null packets added or removed just before the seam; a variable-rate stream keeps its
source timing. Null packets at the seam carry extra PCRs so the PCR stays within 40 ms.
Audio packets move across the seam, so each lap plays the audio of its video, up to a frame
later and never earlier, without drift; the first and last audio PES of a lap are cut on
frame boundaries (AAC ADTS, MPEG audio, AC-3, E-AC-3), and other audio (LATM, S302M, AC-4)
on PES boundaries. An open-GOP or recovery-point loop point is marked as an edit (HEVC CRA
becomes BLA, MPEG-2 gets `broken_link`, a recovery point SEI gets `broken_link_flag`).
SCTE-35 cues are kept only when the whole break, from CUE-OUT to CUE-IN (or CUE-OUT plus
its duration), lies inside the loop; a cue sent ahead of the loop for such a break is moved
to the start. Every lap re-sends the kept cues with `pts_adjustment` advanced and new event
IDs. Other PIDs, such as SMPTE-2038 ANC data or subtitles, keep their packets with their
timestamps advanced.

Interlaced video is read for how its pictures are shown: field pictures (MPEG-2 field
pictures, H.264 PAFF, H.265 field coding) are paired into frames, and a loop never starts
or ends on a second field; the field order of frames (MPEG-2 `top_field_first`, H.264/H.265
`pic_struct`) and pulldown (`repeat_first_field`, `pic_struct` 5-8) must match at both
ends, so a 3:2 pulldown loop holds whole cadence cycles. The frame rate is the rate frames
are shown at: 25 for 1080i25 coded as fields, 29.97 for film with 3:2 pulldown. When both
fields of the loop's first frame are H.265 CRA, both become BLA.

The scan reports the source's continuity errors, corrupt packets (replaced by null
packets), and PES that do not start at a TS packet (refused for video and audio). When the
PAT lists several programs, only the first is looped. An ISDB-T BTS (204-byte packets) is
refused. The input is read whole (`-` reads stdin). The output can be written to a file or
stdout, or sent over UDP (unicast or multicast), paced by the output PCR. The design is described in
[cmd/mp2ts-loop/PLAN.md](cmd/mp2ts-loop/PLAN.md).

**Options:**
- `-scan` - Print the analysis and chosen loop as JSON and exit
- `-o <file>` - Output file, `-` for stdout
- `-a <host:port>` - Output UDP address (unicast or multicast), paced by the output PCR
- `-ttl N` - Multicast TTL for `-a` (default 1)
- `-m N` - Number of laps (default: 1 for file output only, endless with `-a`)
- `-d N` - Longest loop to consider, in ms (`0` = no limit)
- `-fps R` - Frame rate when it cannot be detected, e.g. `25`, `29.97`, or `num/den`; for
  field pictures the frame or field rate, for pulldown the shown or the film rate
- `-write-prepared <file>` - Also write one lap to a file (debug)
- `-indent` - Indent the `-scan` JSON output
- `-q` - No report on stderr

**Examples:**
```sh
# The analysis and chosen loop as JSON
mp2ts-loop -scan input.ts

# Three laps to a file
mp2ts-loop -m 3 -o out.ts input.ts

# Endless, to multicast UDP
mp2ts-loop -a 239.0.0.1:1234 input.ts
```

## How to run

You can download and install any tool directly using

```sh
> go install github.com/Eyevinn/mp2ts-tools/cmd/mp2ts-info@latest
```

If you have the source code you should be able to run a tool like

```sh
> cd cmd/mp2ts-info
> go mod tidy
> go run . h
```

Alternatively, you can use the Makefile to build the tools
or make a coverage check. The `-version` option reports the version Go
embeds from the Git tag and commit (also for `go install`).

## Commits and ChangeLog

This project aims to follow Semantic Versioning and
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/).
There is a manual [ChangeLog](CHANGELOG.md).

## License

MIT, see [LICENSE](LICENSE).

## Support

Join our [community on Slack](http://slack.streamingtech.se) where you can post any questions regarding any of our open source projects. Eyevinn's consulting business can also offer you:

* Further development of this component
* Customization and integration of this component into your platform
* Support and maintenance agreement

Contact [sales@eyevinn.se](mailto:sales@eyevinn.se) if you are interested.

## About Eyevinn Technology

[Eyevinn Technology](https://www.eyevinntechnology.se) is an independent consultant firm specialized in video and streaming. Independent in a way that we are not commercially tied to any platform or technology vendor. As our way to innovate and push the industry forward we develop proof-of-concepts and tools. The things we learn and the code we write we share with the industry in [blogs](https://dev.to/video) and by open sourcing the code we have written.

Want to know more about Eyevinn and how it is to work here. Contact us at <work@eyevinn.se>!

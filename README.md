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

`mp2ts-loop` loops a single-program transport stream seamlessly and forever. It
scans the input for the longest loop bounded by a video random-access point (an
IDR picture, a CRA for open-GOP HEVC, or an MPEG-2 I picture with a sequence
header), keeps the video at a constant frame rate and constant GOP duration,
matches the audio to the loop duration with no accumulated drift, and regenerates
a perfect linear PCR for a constant-rate stream. The loop is a whole number of
frame periods (an even frame count at 59.94 fps, a multiple of four at 23.976),
so the video timestamps continue exactly across the seam. When the loop is not a
whole number of audio frames, the audio is shifted per wrap by less than one
audio frame. For open-GOP HEVC (CRA) streams
the loop-point picture is marked BLA so the seam decodes cleanly. For open-GOP
MPEG-2 streams the loop-point GOP is marked `broken_link`; the B-pictures leading
each seam still predict from the previous wrap's tail, so closed GOPs are needed
for a pixel-perfect seam. Non-audio/video PIDs such as SMPTE-2038 ANC data are passed
through and timestamp-shifted per wrap. SCTE-35 cues are kept only when the whole
break, from CUE-OUT to CUE-IN (or CUE-OUT plus its duration), lies inside the
loop; a cue sent ahead of the loop start for such a break is moved to the start.
Every wrap re-sends the kept cues with `pts_adjustment` advanced by the loop
duration and new event IDs.

In a constant-rate stream every packet keeps its source timing: video and other
packets stay in their source position, and audio that is multiplexed outside the
loop's packets (audio usually lags the video it plays with) is moved across the
loop boundary, to the same place relative to the video. Only just before the
seam are packets moved, by the difference in decoder buffer level between the
two loop points. Variable-rate streams, detected from the PCR (signalled
discontinuities aside), are refused, and a loop never spans a discontinuity. The
output can be written to a file or
sent over UDP (unicast or multicast); UDP output is paced at exactly the rate
the PCR describes, on the monotonic clock.

The input must be a seekable constant-rate file (stdin is not supported, since
it is read more than once), and every audio and video PES must carry a PTS. Supported video is H.264, H.265, and MPEG-2; supported audio is AAC,
MP2, and AC-3. Ultra-low-latency / gradual-decoder-refresh streams (no IDR/CRA) cannot be
looped and are reported as such.

**Options:**
- `-scan` - Scan only: print the loop-point analysis as JSON and exit
- `-o <file>` - Output file path (requires `-m` so the file is bounded)
- `-a <ip:port>` - Output UDP address (unicast or multicast); may loop forever
- `-m N` - Maximum number of loop wraps (`-1` = infinite)
- `-d N` - Target/maximum loop duration in ms (`0` = auto, longest possible)
- `-fps R` - Frame-rate hint, e.g. `25`, `50`, `29.97`, `59.94`, or `num/den`
- `-write-prepared <file>` - Also write the prepared single-loop TS (debug)
- `-indent` - Indent the `-scan` JSON output

**Examples:**
```sh
# Report loop-point candidates and stream analysis
mp2ts-loop -scan input.ts

# Write 3 loops to a file
mp2ts-loop -m 3 -o out.ts input.ts

# Loop forever to multicast UDP
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

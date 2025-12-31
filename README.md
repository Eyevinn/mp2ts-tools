![Test](https://github.com/Eyevinn/mp2ts-tools/workflows/Go/badge.svg)
[![golangci-lint](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml)
[![GoDoc](https://godoc.org/github.com/Eyevinn/mp2ts-tools?status.svg)](http://godoc.org/github.com/Eyevinn/mp2ts-tools)
[![Go Report Card](https://goreportcard.com/badge/github.com/Eyevinn/mp2ts-tools)](https://goreportcard.com/report/github.com/Eyevinn/mp2ts-tools)
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
- PicTiming SEI messages
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

`mp2ts-pslister` shows information about parameter sets (SPS, PPS, and VPS for HEVC) in a TS file. Useful for debugging video codec configurations.

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
or make a coverage check. The Makefile will set the version depending
on the Git commit used.

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

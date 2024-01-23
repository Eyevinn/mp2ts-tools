![Test](https://github.com/Eyevinn/mp2ts-tools/workflows/Go/badge.svg)
[![golangci-lint](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml/badge.svg)](https://github.com/Eyevinn/mp2ts-tools/actions/workflows/golangci-lint.yml)
[![GoDoc](https://godoc.org/github.com/Eyevinn/mp2ts-tools?status.svg)](http://godoc.org/github.com/Eyevinn/mp2ts-tools)
[![Go Report Card](https://goreportcard.com/badge/github.com/Eyevinn/mp2ts-tools)](https://goreportcard.com/report/github.com/Eyevinn/mp2ts-tools)
[![license](https://img.shields.io/github/license/Eyevinn/mp2ts-tools.svg)](https://github.com/Eyevinn/mp2ts-tools/blob/master/LICENSE)

# mp2ts-tools - A collection of tools for MPEG-2 TS

MPEG-2 Transport Stream is a very wide spread format for transporting media.

This repo provides some tools to facilitate analysis and extraction of
data from MPEG-2 TS streams.

## mp2ts-info

`mp2ts-info` is a tool that parses a TS file, or a stream on stdin, and prints
information about the video streams in JSON format.

## mp2ts-pslister

`mp2ts-pslister` is a tool that shows information about SPS, PPS (and VPS in H.265) in a TS file.

## mp2ts-nallister

`mp2ts-nallister` is a tool that shows information about PTS/DTS, PicTiming SEI, and NAL units.

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

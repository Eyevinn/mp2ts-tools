# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

- Nothing yet

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

[Unreleased]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.2.0...HEAD
[v0.2.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Eyevinn/mp2ts-tools/releases/tag/v0.1.0

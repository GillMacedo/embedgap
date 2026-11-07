# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-07

### Added
- Checks `ReadFile`, `Open`, `ReadDir`, `fs.Sub`, `fs.Glob`, `fs.Stat`, `fs.WalkDir`, `http.ServeFileFS` and `ParseFS` paths against `//go:embed` patterns, through `fs.Sub` and across packages.
- Warnings for files left out by the `_` and `.` rule, secrets pulled in by `all:`, and `filepath.Join` paths.
- Exit 2 with no result when the module doesn't build or `go list` output can't be trusted.
- Text and JSON output, and `--fail-on warn|error|none`.
- `checking-go-embed` skill and plugin manifest.

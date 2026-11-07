# 0001: One standard-library Go file

**Decision:** the tool is a single Go file using `go/parser`, `go/types` and `go list`, with no dependencies.

**Why:** people with `//go:embed` already have Go, so `go run` is the whole install, with no npm package or binaries. The skill can run it straight from the plugin folder.

**Alternatives:** `golang.org/x/tools/go/analysis` would make it a vet-style analyzer with full type information, but needs a dependency, and an analyzer sees one package at a time, so following an exported embed variable into another package is harder. An MCP server would add a server for something a script does.

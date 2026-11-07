# embedgap

Claude Code skill and Go CLI that checks the paths a Go program reads from an `embed.FS` against its `//go:embed` patterns.

## Architecture
`plugin/scripts/embedgap.go` holds all the logic in one standard-library file, so `go run` needs no setup. `goList` runs `go list -e -export -test -json`, `load` parses files, finds embed variables and checks `matchPattern` against `go list`, `trackAll` follows embed variables through assignments and `fs.Sub`, and `checkCalls` checks each read. `plugin/skills/checking-go-embed/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet and tests, including `tests/`
- `make lint`: plugin manifest validation
- `make demo`: run on `examples/brokenembed` and `examples/fixedembed`

## Conventions
- Standard library only. Never run the checked module's code.
- Any input it can't fully read or trust exits 2 with no result printed, whatever `--fail-on` says. Add a test in `TestUntrustedSource` or `TestUntrustedGoList` for each new input.
- Each finding code is stable. Add a test for what it flags and for what must stay quiet.
- An ERROR must mean the call fails at run time. If it only might fail, it's a WARN or a note.

## Do and don't
- Do run a change on a real project and on both examples before calling it done.
- Do keep `tests/examples_test.go` passing: errors must match the functions that fail when the example runs.
- Don't print "No problems found" for a run that checked nothing it claims to have checked.

## Definition of done
`make test` and `make lint` pass, README output matches a real run, and `ROADMAP.md` and docs are updated when relevant.

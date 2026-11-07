# 0002: Exit 2 on any input it can't trust

**Decision:** if the module doesn't build, `go list` fails or prints output that isn't valid JSON, a pattern matches nothing, there are no `//go:embed` lines, or embedgap's own reading of the patterns disagrees with `go list`, it exits 2 and prints no result. `--fail-on none` doesn't change this.

**Why:** a checker that prints "No problems found" for a project it couldn't read is worse than none, because CI goes green. Running `go list` with `-export` costs a compile, which is cached after the first run, and catches type errors that parsing alone misses.

**Alternatives:** skipping packages that don't build and checking the rest. That would give partial results that look complete, so it was rejected.

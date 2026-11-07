# Architecture

    goList --> go list -e -export -test -json ./...   (exit 2 on any build or JSON error)
    load   --> parse files, findVars, matchPattern, compare with go list's EmbedFiles (exit 2 on any difference)
    trackAll --> type-check each package, follow embed vars through assignments, fs.Sub and exported vars
    checkCalls --> evalPath on each path argument --> checkPath or checkGlob --> findings
    checkVar --> dropped files and secrets for each embed variable

- **go list is the source of truth for what is embedded.** embedgap applies the embed rules itself so it can explain why a file is missing, such as the `_` and `.` rule, then insists that its file list equals `go list`'s. A gap in its rules becomes exit 2, never a wrong finding.
- **`-export` compiles the module.** That makes type errors, including errors in test files, stop the run, so a broken build can't look clean.
- **Type checking without dependencies.** Each package is type-checked with an importer that returns empty packages, named from `go list`, for packages in the module, and fails for others. Package names resolve, so `app.DB` and `fs.Sub` are recognized even under a `/v2` path or an alias, without loading dependencies.
- **Tracking is conservative.** A variable assigned from two sources, or from anything it can't follow, isn't tracked, and reads through it aren't checked.
- **Paths** are evaluated from constants, `+`, `path.Join`, `filepath.Join` and `fmt.Sprintf`. A path with fixed parts and run-time parts becomes a regular expression that must match some embedded entry.
- **The skill** has Claude read the exit code first, confirm each finding in the code, and say what wasn't checked.

Key choices: see `decisions/`.

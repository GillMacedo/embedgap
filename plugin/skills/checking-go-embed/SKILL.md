---
name: checking-go-embed
description: Checks that the paths a Go program reads from an embed.FS are really embedded, by comparing ReadFile, Open, ReadDir, fs.Sub, fs.Glob, http.ServeFileFS and template.ParseFS calls with the //go:embed patterns and the files on disk. Finds typos, files left out because their names start with _ or ., fs.Sub calls that point at the wrong directory, template patterns that match nothing, filepath.Join paths that break on Windows, and secrets such as .env pulled in by all: patterns. Use when a Go program returns "file does not exist" or 404 for embedded files, when templates fail with "pattern matches no files", or when reviewing changes to //go:embed lines or embedded assets.
---

# Checking Go embed paths

Run the bundled tool from the module root. It needs Go 1.22 or newer and only the standard library. It runs `go list -export`, which compiles the module like `go vet` does, but it never runs the module's code.

    go run "${CLAUDE_PLUGIN_ROOT}/scripts/embedgap.go" --dir PATH

Add `--format json` for machine-readable output.

## Read the exit code first

- **0:** nothing at or above the `--fail-on` level (default `warn`). Read the notes anyway.
- **1:** errors or warnings were found.
- **2:** the input can't be trusted, so nothing was checked. The message says why: the module doesn't build, `go list` failed, a `//go:embed` pattern matches no files, or there are no `//go:embed` lines. Report that to the user and fix the build first. Never describe an exit 2 as a clean result.

A frontend build that hasn't run yet (for example `//go:embed dist` before `npm run build`) also exits 2. Say so, and suggest running the build step first.

## Check each finding

- **ERROR** means the call fails for the paths shown. Open the file at the line, confirm the path, and look at the "did you mean" hint and the `//go:embed` line it names.
- **WARN `dropped`** lists files a directory pattern skips because their names start with `_` or `.`. Ask whether they are needed before suggesting `all:`, because `all:` also pulls in files like `.env`.
- **WARN `secret`** lists files such as `.env` or `id_rsa` that end up inside the binary. Treat it as a leak if the binary is shipped.
- **WARN `filepath-join`** breaks on Windows only. It matters if the program ships for Windows.
- **note `unchecked`** lists paths built at run time that the tool couldn't follow. Read those calls yourself if they matter.
- **note `constraints`** lists files with `//go:embed` that the current GOOS, GOARCH and tags don't build. Rerun with `GOOS=windows` or `GOFLAGS=-tags=...` to check them.

## Report

1. Confirmed failures first, with `file:line`, the path, and the smallest fix: the right path, a wider pattern, or `all:`.
2. Then warnings, and whether each matters for this project.
3. Say what wasn't checked: the notes, other build configurations, and nested modules, which need their own run.

## Don't

Don't edit code or `//go:embed` lines unless the user asks. Don't call the embedded files correct when the result has unchecked paths. Only say the checked paths are fine.

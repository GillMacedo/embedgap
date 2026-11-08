# embedgap: find Go embed.FS reads that fail at run time

[![CI](https://github.com/GillMacedo/embedgap/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/embedgap/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI that checks the paths a Go program reads from an `embed.FS` against its `//go:embed` patterns, so `open x: file does not exist`, a 404 from an embedded file server, or `pattern matches no files` from `template.ParseFS` shows up in CI and not in production.**

`//go:embed` mistakes compile, pass `go vet` and often pass tests, because tests may not read every asset. A typo in a path, a file named `_app.js` that a directory pattern silently skips, an `fs.Sub` pointed at the wrong folder, or a template glob with the wrong extension only fails when that code runs. `embedgap` reads every `ReadFile`, `Open`, `ReadDir`, `fs.Sub`, `fs.Glob`, `fs.WalkDir`, `http.ServeFileFS` and `ParseFS` call on an embed variable, follows the variable through `fs.Sub` and across packages, and compares each path with what `go list` says is embedded.

## What you can ask Claude

- "Why does my embedded file server return 404?"
- "Check that every file this Go service reads from embed.FS is really embedded."
- "Why does template.ParseFS say pattern matches no files?"
- "Does our binary embed a .env file?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/embedgap
    claude plugin install embedgap@embedgap-marketplace

Or run it directly with Go 1.22 or newer. It has no dependencies:

    go run github.com/GillMacedo/embedgap/plugin/scripts@v0.1.0 --dir ./my-module

## Example

`examples/brokenembed` is a small program with five `//go:embed` mistakes. It builds, and `go vet` passes. `go run ./examples/brokenembed` shows four of its six reads failing:

    appJS: open _app/app.js: file does not exist
    staticFiles: open index.html: file does not exist
    page: template: pattern matches no files: `templates/*.tmpl`
    migration: open migration/0001_init.sql: file does not exist
    index: ok
    settings: ok

`embedgap` finds the same failures without running it, plus three warnings. `--fail-on none` keeps the exit code at 0 for this demo:

    $ go run ./plugin/scripts --dir examples/brokenembed --fail-on none
    embedgap examples/brokenembed: 1 package with //go:embed, 4 embed variables, 8 path uses checked
      ERROR main.go:36
            fs.ReadFile("_app/app.js") fails at run time: assets/_app/app.js is left out, because the directory pattern skips names starting with "_", like _app. //go:embed all:assets includes it
            embedded in assets (//go:embed assets at main.go:18), through fs.Sub("assets")
      ERROR main.go:43
            fs.Sub("static") returns no error, but the result is empty, so every read from it fails (an HTTP file server built on it answers 404): there is no directory named static in assets
            embedded in assets (//go:embed assets at main.go:18)
      ERROR main.go:47
            fs.ReadFile("index.html") fails at run time: there is no file named static/index.html in assets
            embedded in assets (//go:embed assets at main.go:18), through fs.Sub("static")
            assets/index.html is embedded, but outside the fs.Sub("static") directory
      ERROR main.go:53
            template.ParseFS("templates/*.tmpl") fails at run time with "pattern matches no files": nothing embedded matches it
            embedded in templates (//go:embed templates/*.html at main.go:21)
            no embedded file ends in .tmpl; embedded files end in .html
      ERROR main.go:59
            migrations.ReadFile("migration/….sql"): no embedded file matches, so this fails for every value
            embedded in migrations (//go:embed migrations/*.sql at main.go:24)
            there is no directory named migration in migrations
      WARN  main.go:18
            //go:embed assets leaves out 1 file on disk:
            assets/_app/ (1 file)
            names starting with _ or . are skipped inside directories; write //go:embed all:assets to include them
      WARN  main.go:27
            //go:embed all:config puts 1 file that usually holds secrets into the binary, where anyone who has the binary can read it:
            config/.npmrc
      WARN  main.go:66
            assets.ReadFile builds its path with filepath.Join, which uses \ on Windows, and embed.FS only accepts /
            use path.Join instead
      5 errors, 3 warnings.

`examples/fixedembed` is the same program with each mistake fixed, and stays quiet:

    $ go run ./plugin/scripts --dir examples/fixedembed
    embedgap examples/fixedembed: 1 package with //go:embed, 4 embed variables, 8 path uses checked
      No problems found in the checked paths.

Add `--format json` for machine-readable output.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Nothing at or above `--fail-on`, which is `warn` by default. Use `--fail-on error` to fail only on errors, or `none` to never fail on findings. |
| 1 | Findings at or above `--fail-on`. |
| 2 | The input can't be trusted, so nothing was checked and no result is printed. This happens when the module doesn't build (including type errors and test files), `go list` fails or prints output that isn't valid JSON, a `//go:embed` pattern matches no files, there are no `//go:embed` lines, or embedgap's reading of the patterns disagrees with `go list`. `--fail-on none` doesn't change this. |

So a broken build can't pass CI as a clean result. In CI:

    go run github.com/GillMacedo/embedgap/plugin/scripts@v0.1.0 --dir .

## What it reports

| Code | Severity | Finds |
|---|---|---|
| `not-embedded` | ERROR | A fixed path that no `//go:embed` pattern includes, with the reason: a typo, a file left out by the `_` and `.` rule, a file in another embed variable, a file on disk the pattern misses, or a directory read as a file |
| `no-match` | ERROR | A `ParseFS` or `fs.Glob` pattern that matches nothing, or a path or glob built at run time, like `"migration/" + name + ".sql"`, whose fixed parts can't match any embedded file |
| `matches-dir` | ERROR | A `ParseFS` pattern that matches a directory, which `ParseFS` fails on |
| `invalid-path` | ERROR | A path with a leading `/` or `./`, a `..` element or a backslash, which `io/fs` always rejects |
| `bad-pattern` | ERROR | A malformed glob |
| `dropped` | WARN | Files a directory pattern skips because their names start with `_` or `.`. `.gitkeep`, `.gitignore`, `.DS_Store` and similar files are left out quietly |
| `secret` | WARN | Files such as `.env`, `.npmrc`, `.netrc` or `id_rsa` that end up in the binary |
| `filepath-join` | WARN | A path built with `filepath.Join`, which uses `\` on Windows |
| `unchecked` | note | Paths built entirely at run time, or patterns passed as a slice, which it can't follow |
| `constraints` | note | Files with `//go:embed` that the current GOOS, GOARCH and build tags leave out |

## How it works

It runs `go list -e -export -test -json ./...` in the directory. `-export` makes the go command compile every package and test, so code that doesn't build stops the run with exit 2. It then applies the go command's embed rules to each pattern itself, and checks that the result matches `go list`'s `EmbedFiles` exactly. Any disagreement stops the run, so a gap in its rules can't turn into a wrong answer. Finally it parses and type-checks each package's source, follows embed variables through assignments, `fs.Sub` and exported package variables, and evaluates path arguments: constants, `+`, `path.Join`, `filepath.Join` and `fmt.Sprintf`.

It never runs the module's code. Compiling does run cgo's C compiler for packages that use cgo, as `go build` does.

## Limits

- **One build configuration per run.** Files whose build constraints don't match the current GOOS, GOARCH and tags are listed in a note, and not checked. Run it again with `GOOS=windows` or `GOFLAGS=-tags=...` to cover them.
- **Paths built at run time are only partly checked.** A path or glob with a fixed part, like `"static/" + name` or `"templates/" + name + ".html"`, is checked for any possible match. A path with no fixed part, a path read from a map or a config file, and `ParseFS(fsys, patterns...)` with a slice are listed in a note, not checked.
- **It follows embed variables through assignments, `fs.Sub` and exported variables.** An `fs.FS` passed as a function argument, stored in a struct field, or assigned from two different sources isn't followed, so reads through it aren't checked.
- **Generated assets must exist first.** A pattern like `//go:embed dist` with no `dist` folder yet is a build error, so it exits 2. Run the frontend build first.
- **Nested modules need their own run,** because `go list ./...` stops at a `go.mod`.
- **The first run on a large module is slow,** because it compiles everything. On Hugo it took about 8 minutes with an empty build cache in the test container, and under 2 seconds once cached.
- **A clean result covers the checked paths only,** not that every embedded file is used or that the files have the right content.

## FAQ

**Why does my Go embed.FS return "file does not exist"?** The path usually doesn't match the `//go:embed` pattern exactly: it needs the pattern's directory prefix, forward slashes, and no leading `/` or `./`. Files inside an embedded directory whose names start with `_` or `.` are skipped unless the pattern uses `all:`. `embedgap` says which of these it is.

**Why does http.FileServer return 404 for embedded files?** Often `fs.Sub` was given a directory that isn't embedded. `fs.Sub` on an `embed.FS` doesn't check that the directory exists, so it returns no error and an empty file system.

**Why doesn't go:embed include files starting with an underscore?** That's the go command's rule for directory patterns. Use `//go:embed all:dir` to include them, and check that it doesn't pull in files like `.env`.

**Doesn't go vet catch this?** No. `go vet` and the compiler check that each pattern matches something, but not the paths the code reads.

**Is it an MCP server?** No. It's a skill with one standard-library Go file, so `go run` is all it needs.

## Tested on

- **Unit tests** in `plugin/scripts` cover each finding, calls that must stay quiet, cross-package reads through a `/v2` module path, embeds in test files, `--fail-on` and every exit code.
- **Untrusted input tests** check that each of these exits 2 and prints no result, under every `--fail-on` and `--format`: an empty, garbage or cut-off Go file, compiler output saved as a `.go` file, type errors in code and in tests, an empty or garbage `go.mod`, a pattern that matches nothing, a missing or empty directory, and a fake `go` command that prints nothing, garbage, cut-off JSON, trailing garbage, fails after printing output, or prints valid JSON whose embedded file list disagrees with the disk.
- **Integration tests** in `tests/` run both example programs and check that the functions `embedgap` reports errors in are exactly the functions that fail at run time, and that the fixed example is quiet and works. Another test checks that a wrong `fs.Sub` gives a file server that answers 404.
- **Real projects:** it ran on goatcounter, miniflux, pocketbase, Hugo and Caddy, and reported no errors or warnings on any of them. ntfy and gotify exit 2, because they embed frontend builds that aren't in the repository. I then changed two paths in goatcounter and one `fs.Sub` in pocketbase to wrong ones, and it caught all three. Running it on goatcounter found a bug in embedgap itself, where reads through a `/v2` import weren't followed, which is now fixed and tested.
- **Go versions:** tests pass on Go 1.22 and Go 1.27 on Linux arm64. CI runs Linux amd64.

Not tested: Windows and macOS, `go.work` workspaces, vendored modules, and a fresh Claude Code session using the skill. The `go run ...@v0.1.0` install line works only once the repository is published.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/embedgap/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `embedgap` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)

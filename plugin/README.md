# embedgap plugin

This plugin adds the `checking-go-embed` skill to Claude Code. The skill runs `scripts/embedgap.go`, which checks that the paths a Go program reads from an `embed.FS` are really embedded: `ReadFile`, `Open`, `ReadDir`, `fs.Sub`, `fs.Glob`, `http.ServeFileFS` and `template.ParseFS` calls are compared with the `//go:embed` patterns and with `go list`. It needs Go 1.22 or newer and nothing else. It compiles the module to make sure it builds, and never runs it.

See the repository README for examples, exit codes and limits: https://github.com/GillMacedo/embedgap

## Install

    claude plugin marketplace add GillMacedo/embedgap
    claude plugin install embedgap@embedgap-marketplace

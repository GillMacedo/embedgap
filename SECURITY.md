# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/embedgap/security/advisories/new), not in a public issue.

`embedgap` runs `go list -export`, which compiles the module it checks, including cgo's C compiler for packages that use cgo. It never runs the module's code. Run it only on code you would build.

A missed or wrong finding is a normal bug. If `embedgap` misses a secret such as a key file being embedded, or reports a clean result for input it couldn't read, report it privately.

# Roadmap

## Next
- Follow an `fs.FS` passed as a function argument or stored in a struct field
- Check globs built at run time, like `ParseFS(fsys, "templates/" + name)`, for any possible match
- A `--goos` list that reruns the check for each platform and merges the notes

## Later
- Report embedded files that nothing reads
- `go.work` workspaces and nested modules in one run

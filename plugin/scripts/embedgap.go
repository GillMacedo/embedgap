// Command embedgap checks that the paths a Go program reads from an embed.FS
// are really embedded. It reports calls such as ReadFile, Open, fs.Sub, fs.Glob
// and template.ParseFS whose path matches nothing in the //go:embed patterns,
// files that a directory pattern leaves out because their names start with
// "_" or ".", and secrets that an all: pattern pulls into the binary.
//
// It uses only the standard library, so it runs with
//
//	go run embedgap.go [flags]
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const version = "0.1.0"

// Exit codes.
const (
	exitClean    = 0
	exitFindings = 1
	exitUnusable = 2
)

// Severities, from most to least severe.
const (
	sevError = "ERROR"
	sevWarn  = "WARN"
	sevNote  = "note"
)

// unusableError means an input couldn't be read fully or trusted. It always
// exits 2 and never prints a summary.
type unusableError struct{ msg string }

func (e *unusableError) Error() string { return e.msg }

func unusable(format string, args ...any) error {
	return &unusableError{msg: fmt.Sprintf(format, args...)}
}

type options struct {
	dir    string
	format string
	failOn string
	goCmd  string
}

// finding is one result line.
type finding struct {
	Severity string   `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Details  []string `json:"details,omitempty"`
}

type report struct {
	Dir       string    `json:"dir"`
	Packages  int       `json:"packages"`
	Variables int       `json:"variables"`
	Checked   int       `json:"checked"`
	Unchecked int       `json:"unchecked"`
	Findings  []finding `json:"findings"`
	Exit      int       `json:"exit"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("embedgap", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var opts options
	fl.StringVar(&opts.dir, "dir", ".", "module directory to check")
	fl.StringVar(&opts.format, "format", "text", "output format: text or json")
	fl.StringVar(&opts.failOn, "fail-on", "warn", "lowest severity that exits 1: warn, error or none")
	showVersion := fl.Bool("version", false, "print the version and exit")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: embedgap [--dir DIR] [--format text|json] [--fail-on warn|error|none]")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return exitUnusable
	}
	if *showVersion {
		fmt.Fprintln(stdout, "embedgap", version)
		return exitClean
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "embedgap: unexpected argument %q; use --dir\n", fl.Arg(0))
		return exitUnusable
	}
	if opts.format != "text" && opts.format != "json" {
		fmt.Fprintf(stderr, "embedgap: --format must be text or json, not %q\n", opts.format)
		return exitUnusable
	}
	switch opts.failOn {
	case "warn", "error", "none":
	default:
		fmt.Fprintf(stderr, "embedgap: --fail-on must be warn, error or none, not %q\n", opts.failOn)
		return exitUnusable
	}
	opts.goCmd = os.Getenv("EMBEDGAP_GO")
	if opts.goCmd == "" {
		opts.goCmd = "go"
	}
	rep, err := check(opts)
	if err != nil {
		fmt.Fprintf(stderr, "embedgap: can't trust the input, so nothing was checked: %v\n", err)
		return exitUnusable
	}
	rep.Exit = exitCode(rep.Findings, opts.failOn)
	if opts.format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		printText(stdout, rep)
	}
	return rep.Exit
}

func exitCode(fs []finding, failOn string) int {
	limit := 0
	switch failOn {
	case "warn":
		limit = sevRank(sevWarn)
	case "error":
		limit = sevRank(sevError)
	default:
		return exitClean
	}
	for _, f := range fs {
		if sevRank(f.Severity) >= limit {
			return exitFindings
		}
	}
	return exitClean
}

func sevRank(s string) int {
	switch s {
	case sevError:
		return 3
	case sevWarn:
		return 2
	}
	return 1
}

func printText(w io.Writer, rep *report) {
	fmt.Fprintf(w, "embedgap %s: %s with //go:embed, %s, %s checked\n",
		rep.Dir, plural(rep.Packages, "package", "packages"), plural(rep.Variables, "embed variable", "embed variables"), plural(rep.Checked, "path use", "path uses"))
	counts := map[string]int{}
	for _, f := range rep.Findings {
		counts[f.Severity]++
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintln(w, strings.TrimRight(fmt.Sprintf("  %-5s %s", f.Severity, loc), " "))
		fmt.Fprintf(w, "        %s\n", f.Message)
		for _, d := range f.Details {
			fmt.Fprintf(w, "        %s\n", d)
		}
	}
	var parts []string
	if n := counts[sevError]; n > 0 {
		parts = append(parts, plural(n, "error", "errors"))
	}
	if n := counts[sevWarn]; n > 0 {
		parts = append(parts, plural(n, "warning", "warnings"))
	}
	if len(parts) == 0 {
		msg := "  No problems found in the checked paths."
		if rep.Checked == 0 {
			msg = "  No reads from an embed.FS were found, so only the //go:embed patterns were checked, and they have no problems."
		}
		if rep.Unchecked > 0 {
			msg += fmt.Sprintf(" Path uses not checked: %d (see the note above).", rep.Unchecked)
		}
		fmt.Fprintln(w, msg)
		return
	}
	fmt.Fprintf(w, "  %s.\n", strings.Join(parts, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---------------------------------------------------------------------------
// go list

// listPkg holds the fields of go list -json output that embedgap uses.
type listPkg struct {
	Dir                string
	ImportPath         string
	Name               string
	ForTest            string
	Module             *struct{ Path, Dir string }
	GoFiles            []string
	CgoFiles           []string
	TestGoFiles        []string
	XTestGoFiles       []string
	IgnoredGoFiles     []string
	InvalidGoFiles     []string
	EmbedPatterns      []string
	EmbedFiles         []string
	TestEmbedPatterns  []string
	TestEmbedFiles     []string
	XTestEmbedPatterns []string
	XTestEmbedFiles    []string
	Error              *struct{ Err string }
	DepsErrors         []*struct{ Err string }
}

func goList(opts options) ([]*listPkg, error) {
	cmd := exec.Command(opts.goCmd, "list", "-e", "-export", "-test", "-json", "./...")
	cmd.Dir = opts.dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, unusable("go list failed in %s: %v\n%s", opts.dir, err, firstLines(errb.String(), 8))
	}
	dec := json.NewDecoder(&out)
	var pkgs []*listPkg
	for {
		var p listPkg
		err := dec.Decode(&p)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, unusable("go list printed output that isn't valid JSON: %v", err)
		}
		if p.ImportPath == "" || p.Dir == "" {
			return nil, unusable("go list printed a package with no import path or directory")
		}
		pkgs = append(pkgs, &p)
	}
	if len(pkgs) == 0 {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = "no packages"
		}
		return nil, unusable("go list found no packages in %s: %s", opts.dir, firstLines(msg, 4))
	}
	// -export compiles every package and test variant, so code that doesn't
	// build shows up here, including errors only the type checker finds.
	for _, p := range pkgs {
		if p.Error != nil {
			return nil, unusable("go list reports an error in package %s, so it doesn't build:\n%s", p.ImportPath, firstLines(p.Error.Err, 6))
		}
		if len(p.DepsErrors) > 0 && p.DepsErrors[0] != nil {
			return nil, unusable("a dependency of package %s doesn't build:\n%s", p.ImportPath, firstLines(p.DepsErrors[0].Err, 6))
		}
	}
	// Keep the packages themselves, not the test variants -test adds.
	var keep []*listPkg
	for _, p := range pkgs {
		if p.ForTest != "" || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		keep = append(keep, p)
	}
	return keep, nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Embed patterns

// dropped is a file or directory that a directory pattern leaves out because
// its name starts with "_" or ".".
type dropped struct {
	rel   string // slash path relative to the package directory
	isDir bool
	files int // regular files inside, for a directory
}

// matchResult is what one //go:embed pattern selects.
type matchResult struct {
	files   []string
	dropped []dropped
}

// matchPattern applies the go command's embed rules to one pattern, relative
// to the package directory. It follows cmd/go's resolveEmbed.
func matchPattern(pkgDir, pattern string) (*matchResult, error) {
	glob, all := strings.CutPrefix(pattern, "all:")
	if _, err := path.Match(glob, ""); err != nil || !validEmbedPattern(glob) {
		return nil, fmt.Errorf("invalid pattern syntax")
	}
	matches, err := filepath.Glob(quoteGlob(pkgDir) + string(filepath.Separator) + filepath.FromSlash(glob))
	if err != nil {
		return nil, err
	}
	res := &matchResult{}
	seen := map[string]bool{}
	for _, m := range matches {
		rel, err := filepath.Rel(pkgDir, m)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		// Every directory between the package and the match must be in this
		// module and have a valid name.
		for d := rel; d != "." && d != ""; d = path.Dir(d) {
			if d != rel {
				if _, err := os.Stat(filepath.Join(pkgDir, filepath.FromSlash(d), "go.mod")); err == nil {
					return nil, fmt.Errorf("cannot embed %s: in different module", rel)
				}
			}
			if badEmbedName(path.Base(d)) {
				return nil, fmt.Errorf("cannot embed %s: invalid name %s", rel, path.Base(d))
			}
		}
		info, err := os.Lstat(m)
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode().IsRegular():
			if !seen[rel] {
				seen[rel] = true
				res.files = append(res.files, rel)
			}
		case info.IsDir():
			if _, err := os.Stat(filepath.Join(m, "go.mod")); err == nil {
				return nil, fmt.Errorf("cannot embed directory %s: in different module", rel)
			}
			count := 0
			err := filepath.WalkDir(m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				r, _ := filepath.Rel(pkgDir, p)
				r = filepath.ToSlash(r)
				name := d.Name()
				if p != m && (badEmbedName(name) || ((name[0] == '.' || name[0] == '_') && !all)) {
					if !badEmbedName(name) {
						dr := dropped{rel: r, isDir: d.IsDir()}
						if d.IsDir() {
							dr.files = countFiles(p)
						}
						res.dropped = append(res.dropped, dr)
					}
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if d.IsDir() {
					if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
						return filepath.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				count++
				if !seen[r] {
					seen[r] = true
					res.files = append(res.files, r)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if count == 0 {
				return nil, fmt.Errorf("cannot embed directory %s: contains no embeddable files", rel)
			}
		default:
			return nil, fmt.Errorf("cannot embed irregular file %s", rel)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no matching files found")
	}
	return res, nil
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

// validEmbedPattern matches cmd/go: a pattern is a valid io/fs path.
func validEmbedPattern(p string) bool {
	return p != "." && fs.ValidPath(p)
}

func quoteGlob(s string) string {
	if filepath.Separator == '\\' {
		// Backslash is the separator on Windows, so it can't escape.
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// badEmbedName reports names the go command never embeds: version control
// directories and names that aren't valid module file paths.
func badEmbedName(name string) bool {
	switch name {
	case "", ".bzr", ".hg", ".git", ".svn":
		return true
	}
	return !validModuleFileName(name)
}

// validModuleFileName approximates golang.org/x/mod/module's file path
// element check. A disagreement with go list stops the run, so an
// approximation can't produce a wrong answer, only exit 2.
func validModuleFileName(elem string) bool {
	if elem == "" || elem == "." || elem == ".." {
		return false
	}
	if elem[len(elem)-1] == '.' {
		return false
	}
	for _, r := range elem {
		ok := false
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			ok = true
		case strings.ContainsRune("!#$%&()+,-.=@[]^_{}~ ", r):
			ok = true
		case r >= utf8.RuneSelf && unicode.IsLetter(r):
			ok = true
		}
		if !ok {
			return false
		}
	}
	short, _, _ := strings.Cut(elem, ".")
	switch strings.ToUpper(short) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return false
	}
	if i := strings.LastIndex(short, "~"); i >= 0 && i < len(short)-1 {
		if _, err := strconv.Atoi(short[i+1:]); err == nil {
			return false
		}
	}
	return true
}

// parseEmbedArgs splits the arguments of a //go:embed line, as go/build does.
func parseEmbedArgs(args string) ([]string, error) {
	var list []string
	for args = strings.TrimSpace(args); args != ""; args = strings.TrimSpace(args) {
		var p string
		switch args[0] {
		case '`':
			i := strings.Index(args[1:], "`")
			if i < 0 {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
			p = args[1 : 1+i]
			args = args[1+i+1:]
		case '"':
			found := false
			for i := 1; i < len(args); i++ {
				if args[i] == '\\' {
					i++
					continue
				}
				if args[i] == '"' {
					q, err := strconv.Unquote(args[:i+1])
					if err != nil {
						return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args[:i+1])
					}
					p = q
					args = args[i+1:]
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
		default:
			i := len(args)
			for j, c := range args {
				if unicode.IsSpace(c) {
					i = j
					break
				}
			}
			p = args[:i]
			args = args[i:]
		}
		if args != "" {
			if c, _ := utf8.DecodeRuneInString(args); !unicode.IsSpace(c) {
				return nil, fmt.Errorf("invalid quoted string in //go:embed: %s", args)
			}
		}
		list = append(list, p)
	}
	return list, nil
}

// ---------------------------------------------------------------------------
// Embed variables

type varKind int

const (
	kindFS varKind = iota
	kindString
	kindBytes
	kindOther
)

// embedVar is one variable with //go:embed directives.
type embedVar struct {
	name     string
	pkg      *pkgState
	test     bool
	file     string // relative to the module directory
	line     int
	kind     varKind
	patterns []string
	files    map[string]bool // slash paths relative to the package directory
	dirs     map[string]bool // every parent directory of files, plus "."
	dropped  []dropped
	all      []string // patterns with an all: prefix
	obj      types.Object
}

func (v *embedVar) describe() string {
	return fmt.Sprintf("%s (//go:embed %s at %s:%d)", v.name, strings.Join(v.patterns, " "), v.file, v.line)
}

// unit is a set of files type-checked together: a package with its internal
// tests, or its external _test package.
type unit struct {
	files  []*ast.File
	names  []string // relative to the module directory
	info   *types.Info
	isTest bool
}

type pkgState struct {
	lp    *listPkg
	rel   string // package directory relative to the module directory
	units []*unit
	vars  []*embedVar
}

// fsRef says where an fs.FS value comes from: an embed variable and a
// directory inside it, after fs.Sub.
type fsRef struct {
	v      *embedVar
	prefix string // "." or a slash path relative to the package directory
}

type checker struct {
	opts      options
	fset      *token.FileSet
	pkgs      []*pkgState
	byPath    map[string]*pkgState
	tracked   map[types.Object]*fsRef
	untracked map[types.Object]bool
	// exported package-level FS values, for use from other packages
	exported map[string]*fsRef // "importpath.Name"
	// per-unit indexes of function parameters that may be followed to the
	// embed variable they receive
	params    map[*unit]*paramIndex
	visiting  map[types.Object]bool // parameter resolutions in progress
	findings  []finding
	checked   int
	unchecked []string
	seen      map[string]bool
}

func check(opts options) (*report, error) {
	abs, err := filepath.Abs(opts.dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, unusable("%s is not a directory", opts.dir)
	}
	opts.dir = abs
	pkgs, err := goList(opts)
	if err != nil {
		return nil, err
	}
	c := &checker{
		opts:      opts,
		fset:      token.NewFileSet(),
		byPath:    map[string]*pkgState{},
		tracked:   map[types.Object]*fsRef{},
		untracked: map[types.Object]bool{},
		exported:  map[string]*fsRef{},
		params:    map[*unit]*paramIndex{},
		visiting:  map[types.Object]bool{},
		seen:      map[string]bool{},
	}
	for _, lp := range pkgs {
		if lp.Error != nil {
			return nil, unusable("go list reports an error in package %s, so it doesn't build:\n%s", lp.ImportPath, firstLines(lp.Error.Err, 6))
		}
		if len(lp.InvalidGoFiles) > 0 {
			return nil, unusable("go list can't read %s in package %s", strings.Join(lp.InvalidGoFiles, ", "), lp.ImportPath)
		}
		rel, err := filepath.Rel(opts.dir, lp.Dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // a package outside the directory, from go.work
		}
		ps := &pkgState{lp: lp, rel: filepath.ToSlash(rel)}
		if err := c.load(ps); err != nil {
			return nil, err
		}
		c.pkgs = append(c.pkgs, ps)
		c.byPath[lp.ImportPath] = ps
	}
	nvars := 0
	npkgs := 0
	for _, ps := range c.pkgs {
		if len(ps.vars) > 0 {
			npkgs++
			nvars += len(ps.vars)
		}
	}
	if nvars == 0 {
		return nil, unusable("found no //go:embed directives in %s (%s), so there is nothing to check", opts.dir, plural(len(c.pkgs), "package", "packages"))
	}
	c.trackAll()
	for _, ps := range c.pkgs {
		for _, u := range ps.units {
			c.checkCalls(ps, u)
		}
		for _, v := range ps.vars {
			c.checkVar(v)
		}
	}
	c.noteIgnored()
	if len(c.unchecked) > 0 {
		details := c.unchecked
		if len(details) > 8 {
			details = append(details[:8:8], fmt.Sprintf("and %d more", len(c.unchecked)-8))
		}
		c.findings = append(c.findings, finding{
			Severity: sevNote, Code: "unchecked",
			Message: fmt.Sprintf("%s built at run time, or passed as a slice, %s checked:", plural(len(c.unchecked), "path use", "path uses"), map[bool]string{true: "wasn't", false: "weren't"}[len(c.unchecked) == 1]),
			Details: details,
		})
	}
	sort.SliceStable(c.findings, func(i, j int) bool {
		a, b := c.findings[i], c.findings[j]
		if sevRank(a.Severity) != sevRank(b.Severity) {
			return sevRank(a.Severity) > sevRank(b.Severity)
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	dir := opts.dir
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, dir); err == nil && !strings.HasPrefix(r, "..") {
			dir = filepath.ToSlash(r)
		}
	}
	findings := c.findings
	if findings == nil {
		findings = []finding{}
	}
	return &report{Dir: dir, Packages: npkgs, Variables: nvars, Checked: c.checked, Unchecked: len(c.unchecked), Findings: findings}, nil
}

func (c *checker) relFile(ps *pkgState, name string) string {
	if ps.rel == "." {
		return name
	}
	return ps.rel + "/" + name
}

// load parses a package's files, finds its embed variables, matches their
// patterns and compares the result with go list.
func (c *checker) load(ps *pkgState) error {
	lp := ps.lp
	groups := []struct {
		files  []string
		isTest bool
	}{
		{append(append(append([]string{}, lp.GoFiles...), lp.CgoFiles...), lp.TestGoFiles...), false},
		{lp.XTestGoFiles, true},
	}
	for gi, g := range groups {
		if len(g.files) == 0 {
			continue
		}
		u := &unit{isTest: gi == 1}
		for _, name := range g.files {
			full := filepath.Join(lp.Dir, name)
			src, err := os.ReadFile(full)
			if err != nil {
				return unusable("can't read %s: %v", c.relFile(ps, name), err)
			}
			f, err := parser.ParseFile(c.fset, full, src, parser.ParseComments)
			if err != nil {
				return unusable("%s isn't valid Go: %v", c.relFile(ps, name), firstLines(err.Error(), 3))
			}
			u.files = append(u.files, f)
			u.names = append(u.names, c.relFile(ps, name))
			isTestFile := strings.HasSuffix(name, "_test.go")
			vars, err := c.findVars(ps, f, src, c.relFile(ps, name), isTestFile)
			if err != nil {
				return err
			}
			ps.vars = append(ps.vars, vars...)
		}
		ps.units = append(ps.units, u)
	}
	// Match patterns and compare with go list.
	var mine, mineTest, patterns, patternsTest []string
	for _, v := range ps.vars {
		for _, p := range v.patterns {
			res, err := matchPattern(lp.Dir, p)
			if err != nil {
				return unusable("//go:embed %s in %s:%d: %v", p, v.file, v.line, err)
			}
			for _, f := range res.files {
				v.files[f] = true
			}
			v.dropped = append(v.dropped, res.dropped...)
			if strings.HasPrefix(p, "all:") {
				v.all = append(v.all, p)
			}
			if v.test {
				patternsTest = append(patternsTest, p)
				mineTest = append(mineTest, res.files...)
			} else {
				patterns = append(patterns, p)
				mine = append(mine, res.files...)
			}
		}
		for f := range v.files {
			for d := path.Dir(f); ; d = path.Dir(d) {
				v.dirs[d] = true
				if d == "." {
					break
				}
			}
		}
		v.dirs["."] = true
	}
	cmp := func(what string, ours, theirs []string) error {
		a, b := uniqSorted(ours), uniqSorted(theirs)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			return unusable("embedgap's reading of %s in package %s disagrees with go list, so its results can't be trusted. go list: %v; embedgap: %v. Please report this", what, lp.ImportPath, b, a)
		}
		return nil
	}
	if err := cmp("//go:embed patterns", patterns, lp.EmbedPatterns); err != nil {
		return err
	}
	if err := cmp("embedded files", mine, lp.EmbedFiles); err != nil {
		return err
	}
	if err := cmp("test //go:embed patterns", patternsTest, append(append([]string{}, lp.TestEmbedPatterns...), lp.XTestEmbedPatterns...)); err != nil {
		return err
	}
	if err := cmp("test embedded files", mineTest, append(append([]string{}, lp.TestEmbedFiles...), lp.XTestEmbedFiles...)); err != nil {
		return err
	}
	return nil
}

func uniqSorted(s []string) []string {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	out := make([]string, 0, len(m))
	for x := range m {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

// findVars finds every //go:embed directive in a file and the variable it
// applies to. A directive the compiler would reject stops the run.
func (c *checker) findVars(ps *pkgState, f *ast.File, src []byte, rel string, test bool) ([]*embedVar, error) {
	type item struct {
		pos  token.Pos
		spec *ast.ValueSpec // nil for anything that isn't a var spec
	}
	var items []item
	embedName := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "embed" {
			if imp.Name != nil {
				embedName = imp.Name.Name
			} else {
				embedName = "embed"
			}
		}
	}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			items = append(items, item{pos: d.Pos()})
			continue
		}
		for i, s := range gd.Specs {
			vs, ok := s.(*ast.ValueSpec)
			pos := s.Pos()
			if i == 0 && !gd.Lparen.IsValid() {
				pos = gd.Pos()
			}
			if ok && gd.Tok == token.VAR {
				items = append(items, item{pos: pos, spec: vs})
			} else {
				items = append(items, item{pos: pos})
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].pos < items[j].pos })
	bySpec := map[*ast.ValueSpec]*embedVar{}
	var vars []*embedVar
	for _, cg := range f.Comments {
		for _, cm := range cg.List {
			text := cm.Text
			if !strings.HasPrefix(text, "//go:embed") {
				continue
			}
			rest := text[len("//go:embed"):]
			if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
				continue // a different directive, such as //go:embedfoo
			}
			line := c.fset.Position(cm.Pos()).Line
			args, err := parseEmbedArgs(rest)
			if err != nil {
				return nil, unusable("%s:%d: %v", rel, line, err)
			}
			if len(args) == 0 {
				return nil, unusable("%s:%d: //go:embed has no patterns", rel, line)
			}
			idx := sort.Search(len(items), func(i int) bool { return items[i].pos > cm.Pos() })
			if idx == len(items) || items[idx].spec == nil || inFunc(f, cm.Pos()) {
				return nil, unusable("%s:%d: //go:embed isn't followed by a package-level var declaration, which the compiler rejects", rel, line)
			}
			// Only blank lines and comments may come between them.
			between := src[c.fset.Position(cm.End()).Offset:c.fset.Position(items[idx].pos).Offset]
			if !onlyCommentsAndSpace(between) {
				return nil, unusable("%s:%d: //go:embed isn't directly above a var declaration, which the compiler rejects", rel, line)
			}
			vs := items[idx].spec
			if len(vs.Names) != 1 || len(vs.Values) != 0 {
				return nil, unusable("%s:%d: //go:embed must apply to one variable with no initializer", rel, line)
			}
			v := bySpec[vs]
			if v == nil {
				v = &embedVar{
					name: vs.Names[0].Name, pkg: ps, test: test, file: rel,
					line:  c.fset.Position(vs.Names[0].Pos()).Line,
					kind:  typeKind(vs.Type, embedName),
					files: map[string]bool{}, dirs: map[string]bool{},
				}
				bySpec[vs] = v
				vars = append(vars, v)
			}
			v.patterns = append(v.patterns, args...)
		}
	}
	return vars, nil
}

func inFunc(f *ast.File, pos token.Pos) bool {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && fd.Body.Pos() < pos && pos < fd.Body.End() {
			return true
		}
	}
	return false
}

var commentOrSpace = regexp.MustCompile(`^(\s|//[^\n]*)*$`)

func onlyCommentsAndSpace(b []byte) bool { return commentOrSpace.Match(b) }

func typeKind(t ast.Expr, embedName string) varKind {
	switch x := t.(type) {
	case *ast.Ident:
		if x.Name == "string" {
			return kindString
		}
	case *ast.ArrayType:
		if id, ok := x.Elt.(*ast.Ident); ok && x.Len == nil && (id.Name == "byte" || id.Name == "uint8") {
			return kindBytes
		}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && embedName != "" && id.Name == embedName && x.Sel.Name == "FS" {
			return kindFS
		}
	}
	return kindOther
}

// ---------------------------------------------------------------------------
// Following fs.FS values

// modImporter returns an empty package with the right name for packages in
// the module, so a selector like goatcounter.DB resolves even when the import
// path ends in /v2. Other imports fail, and go/types guesses their name from
// the path, which is right for the standard library packages embedgap reads.
type modImporter struct {
	byPath map[string]*pkgState
	cache  map[string]*types.Package
}

func (m *modImporter) Import(path string) (*types.Package, error) {
	if p, ok := m.cache[path]; ok {
		return p, nil
	}
	ps := m.byPath[path]
	if ps == nil {
		return nil, errors.New("not loaded")
	}
	p := types.NewPackage(path, ps.lp.Name)
	p.MarkComplete() // go/types drops an incomplete package
	m.cache[path] = p
	return p, nil
}

// trackAll type-checks every unit without its imports, then follows embed
// variables through assignments and fs.Sub across the whole module.
func (c *checker) trackAll() {
	imp := &modImporter{byPath: c.byPath, cache: map[string]*types.Package{}}
	for _, ps := range c.pkgs {
		for _, u := range ps.units {
			u.info = &types.Info{
				Types: map[ast.Expr]types.TypeAndValue{},
				Defs:  map[*ast.Ident]types.Object{},
				Uses:  map[*ast.Ident]types.Object{},
			}
			conf := types.Config{Importer: imp, Error: func(error) {}, FakeImportC: true}
			path := ps.lp.ImportPath
			if u.isTest {
				path += "_test"
			}
			conf.Check(path, c.fset, u.files, u.info)
		}
		for _, v := range ps.vars {
			if v.kind != kindFS {
				continue
			}
			for _, u := range ps.units {
				for id, obj := range u.info.Defs {
					if obj != nil && id.Name == v.name && c.fset.Position(id.Pos()).Line == v.line && strings.HasSuffix(c.fset.Position(id.Pos()).Filename, filepath.FromSlash(path.Base(v.file))) {
						if _, ok := obj.(*types.Var); ok && obj.Parent() == obj.Pkg().Scope() {
							v.obj = obj
						}
					}
				}
			}
			if v.obj != nil {
				ref := &fsRef{v: v, prefix: "."}
				c.tracked[v.obj] = ref
				if !v.test && ast.IsExported(v.name) {
					c.exported[ps.lp.ImportPath+"."+v.name] = ref
				}
			}
		}
	}
	// Assignments: x := embedVar, x, err := fs.Sub(embedVar, "dir"),
	// x := must(fs.Sub(...)). Repeat until nothing changes, since one
	// derived value can feed another, in another package.
	type assign struct {
		u   *unit
		ps  *pkgState
		lhs types.Object
		rhs ast.Expr // nil when the value can't be followed
	}
	var assigns []assign
	for _, ps := range c.pkgs {
		for _, u := range ps.units {
			for _, f := range u.files {
				ast.Inspect(f, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.AssignStmt:
						c.collectAssign(u, x.Lhs, x.Rhs, func(o types.Object, e ast.Expr) { assigns = append(assigns, assign{u, ps, o, e}) })
					case *ast.ValueSpec:
						if len(x.Values) == 0 {
							// var x T declares a zero value, which assigns nothing.
							return true
						}
						lhs := make([]ast.Expr, len(x.Names))
						for i, id := range x.Names {
							lhs[i] = id
						}
						c.collectAssign(u, lhs, x.Values, func(o types.Object, e ast.Expr) { assigns = append(assigns, assign{u, ps, o, e}) })
					case *ast.UnaryExpr:
						// &x lets anything change x.
						if x.Op == token.AND {
							if id, ok := x.X.(*ast.Ident); ok {
								if o := u.info.Uses[id]; o != nil {
									assigns = append(assigns, assign{u, ps, o, nil})
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	for round := 0; round < 8; round++ {
		changed := false
		sources := map[types.Object][]*fsRef{}
		unknown := map[types.Object]bool{}
		for _, a := range assigns {
			if a.rhs == nil {
				unknown[a.lhs] = true
				continue
			}
			ref := c.resolveFS(a.u, a.rhs)
			if ref == nil {
				unknown[a.lhs] = true
				continue
			}
			sources[a.lhs] = append(sources[a.lhs], ref)
		}
		for obj, refs := range sources {
			if c.untracked[obj] || unknown[obj] {
				continue
			}
			if _, isEmbed := c.tracked[obj]; isEmbed && c.tracked[obj].v.obj == obj {
				continue
			}
			first := refs[0]
			same := true
			for _, r := range refs[1:] {
				if r.v != first.v || r.prefix != first.prefix {
					same = false
				}
			}
			if !same {
				c.untracked[obj] = true
				delete(c.tracked, obj)
				changed = true
				continue
			}
			if old := c.tracked[obj]; old == nil || old.v != first.v || old.prefix != first.prefix {
				c.tracked[obj] = first
				changed = true
				if obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope() && obj.Exported() {
					c.exported[obj.Pkg().Path()+"."+obj.Name()] = first
				}
			}
		}
		// An embed variable reassigned to something we can't follow, or any
		// derived value with an unknown source, is dropped.
		for obj := range unknown {
			if _, ok := c.tracked[obj]; ok || !c.untracked[obj] {
				if _, ok := c.tracked[obj]; ok {
					delete(c.tracked, obj)
					changed = true
				}
				c.untracked[obj] = true
			}
		}
		if !changed {
			break
		}
	}
}

func (c *checker) collectAssign(u *unit, lhs, rhs []ast.Expr, add func(types.Object, ast.Expr)) {
	obj := func(e ast.Expr) types.Object {
		id, ok := e.(*ast.Ident)
		if !ok || id.Name == "_" {
			return nil
		}
		if o := u.info.Defs[id]; o != nil {
			return o
		}
		return u.info.Uses[id]
	}
	switch {
	case len(lhs) == len(rhs):
		for i := range lhs {
			if o := obj(lhs[i]); o != nil {
				add(o, rhs[i])
			}
		}
	case len(rhs) == 1 && len(lhs) == 2:
		// x, err := fs.Sub(...)
		if o := obj(lhs[0]); o != nil {
			if call, ok := unparen(rhs[0]).(*ast.CallExpr); ok && c.isPkgFunc(u, call, "io/fs", "Sub") {
				add(o, rhs[0])
			} else {
				add(o, nil)
			}
		}
		if o := obj(lhs[1]); o != nil {
			add(o, nil)
		}
	default:
		for _, l := range lhs {
			if o := obj(l); o != nil {
				add(o, nil)
			}
		}
	}
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// pkgFunc returns the import path and name of a call to a package-level
// function, such as fs.Sub.
func pkgFunc(u *unit, call *ast.CallExpr) (string, string) {
	sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", ""
	}
	pn, ok := u.info.Uses[id].(*types.PkgName)
	if !ok {
		return "", ""
	}
	return pn.Imported().Path(), sel.Sel.Name
}

func (c *checker) isPkgFunc(u *unit, call *ast.CallExpr, pkg, name string) bool {
	p, n := pkgFunc(u, call)
	return p == pkg && n == name
}

// paramSlot is a parameter of a plain function, by position in its signature.
type paramSlot struct {
	fn    *types.Func
	index int // position among the signature's parameters
	nargs int // number of parameters, for matching call sites
}

// paramIndex records, for one unit, the parameters of its unexported
// non-variadic plain functions, the direct call sites of those functions, and
// the functions used as values, whose call sites no index can cover.
type paramIndex struct {
	params  map[types.Object]*paramSlot
	calls   map[types.Object][]*ast.CallExpr
	escaped map[types.Object]bool
}

// variadic reports whether a function type ends in ...T.
func variadic(ft *ast.FuncType) bool {
	if len(ft.Params.List) == 0 {
		return false
	}
	_, ok := ft.Params.List[len(ft.Params.List)-1].Type.(*ast.Ellipsis)
	return ok
}

// unitParams builds the parameter index of a unit once.
func (c *checker) unitParams(u *unit) *paramIndex {
	if idx := c.params[u]; idx != nil {
		return idx
	}
	idx := &paramIndex{
		params:  map[types.Object]*paramSlot{},
		calls:   map[types.Object][]*ast.CallExpr{},
		escaped: map[types.Object]bool{},
	}
	c.params[u] = idx
	isCallee := map[*ast.Ident]bool{}
	for _, f := range u.files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				if x.Recv != nil || x.Body == nil || ast.IsExported(x.Name.Name) || variadic(x.Type) || x.Type.Params == nil {
					return true
				}
				fn, _ := u.info.Defs[x.Name].(*types.Func)
				if fn == nil {
					return true
				}
				n := 0
				for _, field := range x.Type.Params.List {
					if len(field.Names) == 0 {
						n++
					} else {
						n += len(field.Names)
					}
				}
				i := 0
				for _, field := range x.Type.Params.List {
					if len(field.Names) == 0 {
						i++
						continue
					}
					for _, id := range field.Names {
						if o := u.info.Defs[id]; o != nil {
							idx.params[o] = &paramSlot{fn: fn, index: i, nargs: n}
						}
						i++
					}
				}
			case *ast.CallExpr:
				if id, ok := unparen(x.Fun).(*ast.Ident); ok {
					if o := u.info.Uses[id]; o != nil {
						idx.calls[o] = append(idx.calls[o], x)
						isCallee[id] = true
					}
				}
			}
			return true
		})
	}
	for _, f := range u.files {
		ast.Inspect(f, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || isCallee[id] {
				return true
			}
			if fn, ok := u.info.Uses[id].(*types.Func); ok {
				idx.escaped[fn] = true
			}
			return true
		})
	}
	return idx
}

// resolveParam follows a function parameter used as an fs.FS operand. When
// every call site of the function in the package passes the same embed
// variable in the same directory, reads through the parameter are reads of
// that variable; any disagreement keeps them unchecked.
func (c *checker) resolveParam(u *unit, o types.Object) *fsRef {
	idx := c.unitParams(u)
	slot := idx.params[o]
	if slot == nil || idx.escaped[slot.fn] || c.visiting[o] {
		return nil
	}
	sites := idx.calls[slot.fn]
	if len(sites) == 0 {
		return nil
	}
	c.visiting[o] = true
	defer delete(c.visiting, o)
	var ref *fsRef
	for _, call := range sites {
		if call.Ellipsis.IsValid() || len(call.Args) != slot.nargs {
			return nil
		}
		arg := call.Args[slot.index]
		if id, ok := unparen(arg).(*ast.Ident); ok && u.info.Uses[id] == o {
			continue // the function passes its own parameter on
		}
		r := c.resolveFS(u, arg)
		if r == nil {
			return nil
		}
		if ref == nil {
			ref = r
			continue
		}
		if ref.v != r.v || ref.prefix != r.prefix {
			return nil
		}
	}
	return ref
}

// resolveFS says which embed variable and directory an expression refers to,
// or nil if it can't tell.
func (c *checker) resolveFS(u *unit, e ast.Expr) *fsRef {
	e = unparen(e)
	switch x := e.(type) {
	case *ast.Ident:
		o := u.info.Uses[x]
		if o == nil || c.untracked[o] {
			return nil
		}
		if ref := c.tracked[o]; ref != nil {
			return ref
		}
		return c.resolveParam(u, o)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return nil
		}
		pn, ok := u.info.Uses[id].(*types.PkgName)
		if !ok {
			return nil
		}
		return c.exported[pn.Imported().Path()+"."+x.Sel.Name]
	case *ast.CallExpr:
		if c.isPkgFunc(u, x, "io/fs", "Sub") && len(x.Args) == 2 {
			base := c.resolveFS(u, x.Args[0])
			if base == nil {
				return nil
			}
			p := c.evalPath(u, x.Args[1])
			if p.kind != pathConst || !fs.ValidPath(p.value) {
				return nil
			}
			return &fsRef{v: base.v, prefix: joinPrefix(base.prefix, p.value)}
		}
		// must(fs.Sub(...)) and similar one-argument wrappers.
		if len(x.Args) == 1 {
			if inner, ok := unparen(x.Args[0]).(*ast.CallExpr); ok && c.isPkgFunc(u, inner, "io/fs", "Sub") {
				return c.resolveFS(u, inner)
			}
		}
	}
	return nil
}

func joinPrefix(prefix, p string) string {
	if p == "." {
		return prefix
	}
	if prefix == "." {
		return p
	}
	return prefix + "/" + p
}

// ---------------------------------------------------------------------------
// Path expressions

type pathKind int

const (
	pathUnknown pathKind = iota
	pathConst
	pathPartial // fixed parts with run-time parts between them
)

type pathVal struct {
	kind     pathKind
	value    string   // for pathConst
	parts    []string // for pathPartial: fixed text, with a run-time part between each pair
	filepath bool     // built with filepath.Join
}

func (p pathVal) display() string {
	if p.kind == pathConst {
		return strconv.Quote(p.value)
	}
	return strconv.Quote(strings.Join(p.parts, "…"))
}

func (c *checker) evalPath(u *unit, e ast.Expr) pathVal {
	e = unparen(e)
	if tv, ok := u.info.Types[e]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
		return pathVal{kind: pathConst, value: constant.StringVal(tv.Value)}
	}
	switch x := e.(type) {
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return concat(c.evalPath(u, x.X), c.evalPath(u, x.Y))
		}
	case *ast.CallExpr:
		pkg, name := pkgFunc(u, x)
		switch {
		case (pkg == "path" || pkg == "path/filepath") && name == "Join":
			var out pathVal
			any := false
			for i, a := range x.Args {
				v := c.evalPath(u, a)
				if v.kind == pathConst && v.value == "" {
					continue
				}
				if any {
					out = concat(out, pathVal{kind: pathConst, value: "/"})
				}
				if i == 0 && v.kind == pathConst {
					out = v
				} else {
					out = concat(out, v)
				}
				any = true
			}
			if out.kind == pathConst {
				out.value = path.Clean(out.value)
			}
			out.filepath = pkg == "path/filepath"
			return out
		case pkg == "fmt" && name == "Sprintf" && len(x.Args) >= 1:
			f := c.evalPath(u, x.Args[0])
			if f.kind != pathConst {
				return pathVal{}
			}
			return sprintfPattern(f.value)
		}
	}
	return pathVal{kind: pathUnknown}
}

func concat(a, b pathVal) pathVal {
	toParts := func(v pathVal) []string {
		switch v.kind {
		case pathConst:
			return []string{v.value}
		case pathPartial:
			return v.parts
		}
		return []string{"", ""}
	}
	if a.kind == pathConst && b.kind == pathConst {
		return pathVal{kind: pathConst, value: a.value + b.value, filepath: a.filepath || b.filepath}
	}
	pa, pb := toParts(a), toParts(b)
	parts := append(append([]string{}, pa[:len(pa)-1]...), pa[len(pa)-1]+pb[0])
	parts = append(parts, pb[1:]...)
	out := pathVal{kind: pathPartial, parts: parts, filepath: a.filepath || b.filepath}
	if strings.Join(parts, "") == "" {
		out.kind = pathUnknown
	}
	return out
}

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z%]`)

func sprintfPattern(format string) pathVal {
	locs := verbRe.FindAllStringIndex(format, -1)
	if len(locs) == 0 {
		return pathVal{kind: pathConst, value: format}
	}
	var parts []string
	last := 0
	for _, l := range locs {
		if format[l[0]:l[1]] == "%%" {
			continue
		}
		parts = append(parts, strings.ReplaceAll(format[last:l[0]], "%%", "%"))
		last = l[1]
	}
	parts = append(parts, strings.ReplaceAll(format[last:], "%%", "%"))
	if len(parts) == 1 {
		return pathVal{kind: pathConst, value: parts[0]}
	}
	if strings.Join(parts, "") == "" {
		return pathVal{}
	}
	return pathVal{kind: pathPartial, parts: parts}
}

func (p pathVal) regexp() *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i, part := range p.parts {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// globRegexp turns the fixed parts of a glob built at run time into a regular
// expression. Glob characters in the fixed parts are translated, and each
// run-time part matches anything.
func globRegexp(parts []string) (*regexp.Regexp, bool) {
	var b strings.Builder
	b.WriteString("^")
	for i, part := range parts {
		if i > 0 {
			b.WriteString(".*")
		}
		if !appendGlobRegex(&b, part) {
			return nil, false
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, false
	}
	return re, true
}

// appendGlobRegex writes the regular expression for one fixed part of a glob,
// following the path.Match syntax.
func appendGlobRegex(b *strings.Builder, s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '\\':
			if i >= len(s) {
				return false
			}
			c, n := utf8.DecodeRuneInString(s[i:])
			i += n
			b.WriteString(regexp.QuoteMeta(string(c)))
		case '[':
			var ok bool
			if i, ok = appendGlobClass(b, s, i); !ok {
				return false
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return true
}

// appendGlobClass writes a path.Match character class and returns the index
// after it.
func appendGlobClass(b *strings.Builder, s string, i int) (int, bool) {
	b.WriteByte('[')
	if i < len(s) && s[i] == '^' {
		b.WriteByte('^')
		i++
	}
	n := 0
	for {
		if i >= len(s) {
			return i, false
		}
		if s[i] == ']' {
			if n == 0 {
				return i, false // an empty class is invalid
			}
			b.WriteByte(']')
			return i + 1, true
		}
		lo, next, ok := globClassChar(s, i)
		if !ok {
			return i, false
		}
		i = next
		n++
		if i < len(s) && s[i] == '-' && i+1 < len(s) && s[i+1] != ']' {
			hi, next, ok := globClassChar(s, i+1)
			if !ok {
				return i, false
			}
			i = next
			writeClassChar(b, lo)
			b.WriteByte('-')
			writeClassChar(b, hi)
		} else {
			writeClassChar(b, lo)
		}
	}
}

// globClassChar reads one character of a class, including a backslash escape.
func globClassChar(s string, i int) (rune, int, bool) {
	if i >= len(s) || s[i] == ']' || s[i] == '-' {
		return 0, i, false
	}
	if s[i] == '\\' {
		i++
		if i >= len(s) {
			return 0, i, false
		}
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && n == 1 {
		return 0, i, false
	}
	return r, i + n, true
}

// writeClassChar writes one character for a regular expression class.
func writeClassChar(b *strings.Builder, r rune) {
	if strings.ContainsRune(`\^-]`, r) {
		b.WriteByte('\\')
	}
	b.WriteRune(r)
}

// ---------------------------------------------------------------------------
// Checking calls

type want int

const (
	wantFile want = iota
	wantDir
	wantAny
	wantGlob
	wantParseFS
)

func (c *checker) checkCalls(ps *pkgState, u *unit) {
	for fi, f := range u.files {
		fileName := u.names[fi]
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := pkgFunc(u, call)
			var fsArg ast.Expr
			var paths []ast.Expr
			var w want
			label := ""
			switch {
			case pkg == "io/fs":
				switch name {
				case "ReadFile", "ReadDir", "Stat", "Sub", "Glob", "WalkDir":
					if len(call.Args) < 2 {
						return true
					}
					fsArg, paths = call.Args[0], call.Args[1:2]
					w = map[string]want{"ReadFile": wantFile, "ReadDir": wantDir, "Stat": wantAny, "Sub": wantDir, "Glob": wantGlob, "WalkDir": wantAny}[name]
					label = "fs." + name
				default:
					return true
				}
			case pkg == "net/http" && name == "ServeFileFS":
				if len(call.Args) != 4 {
					return true
				}
				fsArg, paths, w, label = call.Args[2], call.Args[3:4], wantAny, "http.ServeFileFS"
			case (pkg == "html/template" || pkg == "text/template") && name == "ParseFS":
				if len(call.Args) < 2 {
					return true
				}
				fsArg, paths, w, label = call.Args[0], call.Args[1:], wantParseFS, "template.ParseFS"
			case pkg == "":
				sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "ReadFile", "Open", "ReadDir":
					if len(call.Args) != 1 {
						return true
					}
					fsArg, paths = sel.X, call.Args
					w = map[string]want{"ReadFile": wantFile, "Open": wantAny, "ReadDir": wantDir}[sel.Sel.Name]
				case "ParseFS":
					if len(call.Args) < 2 {
						return true
					}
					fsArg, paths, w = call.Args[0], call.Args[1:], wantParseFS
				default:
					return true
				}
				label = exprString(sel.X) + "." + sel.Sel.Name
				if sel.Sel.Name == "ParseFS" {
					label = "ParseFS"
				}
			default:
				return true
			}
			ref := c.resolveFS(u, fsArg)
			if ref == nil {
				return true
			}
			if call.Ellipsis.IsValid() {
				// ParseFS(fsys, patterns...) with a slice: can't see the values.
				c.unchecked = append(c.unchecked, fmt.Sprintf("%s:%d %s", fileName, c.fset.Position(call.Pos()).Line, label))
				return true
			}
			for _, pe := range paths {
				c.checkPath(u, fileName, call, label, ref, pe, w)
			}
			return true
		})
	}
}

func exprString(e ast.Expr) string {
	switch x := unparen(e).(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	}
	return "fs"
}

func (c *checker) add(f finding) {
	key := fmt.Sprintf("%s|%d|%s|%s", f.File, f.Line, f.Code, f.Message)
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.findings = append(c.findings, f)
}

// entries lists the paths of an embed variable as seen through ref: files
// and directories relative to ref's root.
func entries(ref *fsRef, w want) []string {
	var out []string
	addFrom := func(m map[string]bool) {
		for p := range m {
			switch {
			case ref.prefix == ".":
				out = append(out, p)
			case p == ref.prefix:
				out = append(out, ".")
			case strings.HasPrefix(p, ref.prefix+"/"):
				out = append(out, p[len(ref.prefix)+1:])
			}
		}
	}
	if w != wantDir {
		addFrom(ref.v.files)
	}
	if w != wantFile {
		addFrom(ref.v.dirs)
	}
	sort.Strings(out)
	return out
}

func (c *checker) checkPath(u *unit, fileName string, call *ast.CallExpr, label string, ref *fsRef, pe ast.Expr, w want) {
	line := c.fset.Position(pe.Pos()).Line
	pv := c.evalPath(u, pe)
	where := ref.v.describe()
	if ref.prefix != "." {
		where = fmt.Sprintf("%s, through fs.Sub(%q)", where, ref.prefix)
	}
	if pv.kind == pathUnknown {
		c.unchecked = append(c.unchecked, fmt.Sprintf("%s:%d %s", fileName, line, label))
		return
	}
	c.checked++
	if pv.filepath && (pv.kind == pathConst && pv.value != "" || pv.kind == pathPartial) {
		c.add(finding{Severity: sevWarn, File: fileName, Line: line, Code: "filepath-join",
			Message: fmt.Sprintf("%s builds its path with filepath.Join, which uses \\ on Windows, and embed.FS only accepts /", label),
			Details: []string{"use path.Join instead"}})
	}
	if pv.kind == pathConst && (w == wantGlob || w == wantParseFS) {
		c.checkGlob(fileName, line, label, ref, where, pv.value, w)
		return
	}
	if pv.kind == pathPartial {
		if w == wantGlob || w == wantParseFS {
			// A glob built at run time: check that some embedded path can match.
			re, ok := globRegexp(pv.parts)
			if ok {
				for _, e := range entries(ref, wantAny) {
					if re.MatchString(e) {
						c.unchecked = append(c.unchecked, fmt.Sprintf("%s:%d %s", fileName, line, label))
						c.checked--
						return
					}
				}
				verb := "returns no matches and no error"
				if w == wantParseFS {
					verb = "fails at run time with \"pattern matches no files\""
				}
				c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "no-match",
					Message: fmt.Sprintf("%s(%s) %s: nothing embedded can match it, whatever the run-time part is", label, pv.display(), verb),
					Details: []string{"embedded in " + where}})
				return
			}
			c.unchecked = append(c.unchecked, fmt.Sprintf("%s:%d %s", fileName, line, label))
			c.checked--
			return
		}
		if strings.HasPrefix(pv.parts[0], "/") || strings.HasPrefix(pv.parts[0], "./") {
			c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "invalid-path",
				Message: fmt.Sprintf("%s(%s) always fails: io/fs paths can't start with / or ./", label, pv.display()),
				Details: []string{"embedded paths are relative to the package directory, with no leading slash: " + strings.TrimLeft(strings.TrimPrefix(pv.parts[0], "./"), "/") + "…"}})
			return
		}
		re := pv.regexp()
		for _, e := range entries(ref, w) {
			if re.MatchString(e) {
				return
			}
		}
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "no-match",
			Message: fmt.Sprintf("%s(%s): no embedded %s matches, so this fails for every value", label, pv.display(), wantNoun(w)),
			Details: append([]string{"embedded in " + where}, c.suggest(ref, pv.parts[0], w)...)})
		return
	}
	p := pv.value
	if strings.Contains(p, `\`) {
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "invalid-path",
			Message: fmt.Sprintf("%s(%s) always fails: embed.FS paths use /, not \\", label, pv.display()),
			Details: []string{"use " + strconv.Quote(strings.ReplaceAll(p, `\`, "/"))}})
		return
	}
	if !fs.ValidPath(p) {
		fixed := strings.Trim(path.Clean("/"+p), "/")
		if fixed == "" {
			fixed = "."
		}
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "invalid-path",
			Message: fmt.Sprintf("%s(%s) always fails: io/fs paths can't start or end with /, or contain . or .. elements", label, pv.display()),
			Details: []string{"use " + strconv.Quote(fixed)}})
		return
	}
	full := joinPrefix(ref.prefix, p)
	if c.has(ref.v, full, w) {
		return
	}
	verb := "fails at run time"
	if label == "fs.Sub" {
		verb = "returns no error, but the result is empty, so every read from it fails (an HTTP file server built on it answers 404)"
	}
	if label == "fs.Glob" {
		verb = "returns no matches and no error"
	}
	msg := fmt.Sprintf("%s(%s) %s: %s", label, pv.display(), verb, c.whyMissing(ref.v, full, w))
	details := []string{"embedded in " + where}
	details = append(details, c.suggestFull(ref, full, w)...)
	c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "not-embedded", Message: msg, Details: details})
}

func wantNoun(w want) string {
	switch w {
	case wantFile:
		return "file"
	case wantDir:
		return "directory"
	}
	return "path"
}

func (c *checker) has(v *embedVar, full string, w want) bool {
	switch w {
	case wantFile:
		return v.files[full]
	case wantDir:
		return v.dirs[full]
	}
	return v.files[full] || v.dirs[full]
}

// whyMissing explains why a path isn't in an embed variable.
func (c *checker) whyMissing(v *embedVar, full string, w want) string {
	if w == wantFile && v.dirs[full] {
		return full + " is a directory, not a file"
	}
	if w == wantDir && v.files[full] {
		return full + " is a file, not a directory"
	}
	for _, d := range v.dropped {
		if full == d.rel || strings.HasPrefix(full, d.rel+"/") {
			elem := path.Base(d.rel)
			return fmt.Sprintf("%s is left out, because the directory pattern skips names starting with %q, like %s. //go:embed all:%s includes it", full, elem[:1], elem, firstDirPattern(v, d.rel))
		}
	}
	for _, o := range v.pkg.vars {
		if o != v && o.test == v.test && c.has(o, full, w) {
			return fmt.Sprintf("%s is embedded in %s (%s:%d), not in %s", full, o.name, o.file, o.line, v.name)
		}
	}
	disk := filepath.Join(v.pkg.lp.Dir, filepath.FromSlash(full))
	if st, err := os.Stat(disk); err == nil {
		kind := "file"
		if st.IsDir() {
			kind = "directory"
		}
		return fmt.Sprintf("the %s %s exists, but //go:embed %s doesn't include it", kind, full, strings.Join(v.patterns, " "))
	}
	return fmt.Sprintf("there is no %s named %s in %s", wantNoun(w), full, v.name)
}

func firstDirPattern(v *embedVar, rel string) string {
	for _, p := range v.patterns {
		g := strings.TrimPrefix(p, "all:")
		if rel == g || strings.HasPrefix(rel, g+"/") {
			return g
		}
		if ok, _ := path.Match(g, rel); ok {
			return g
		}
		// The match was a parent directory of rel.
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			if ok, _ := path.Match(g, d); ok {
				return g
			}
		}
	}
	return strings.TrimPrefix(v.patterns[0], "all:")
}

func (c *checker) checkGlob(fileName string, line int, label string, ref *fsRef, where, pat string, w want) {
	if _, err := path.Match(pat, ""); err != nil {
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "bad-pattern",
			Message: fmt.Sprintf("%s(%q) always fails: the pattern is malformed", label, pat)})
		return
	}
	// fs.Glob in an embed.FS matches files and directories.
	var matched, dirs []string
	for _, e := range entries(ref, wantAny) {
		if ok, _ := path.Match(pat, e); ok {
			matched = append(matched, e)
			if ref.v.dirs[joinPrefix(ref.prefix, e)] {
				dirs = append(dirs, e)
			}
		}
	}
	if len(matched) == 0 {
		verb := "returns no matches and no error"
		if w == wantParseFS {
			verb = "fails at run time with \"pattern matches no files\""
		}
		details := []string{"embedded in " + where}
		details = append(details, c.globHint(ref, pat)...)
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "no-match",
			Message: fmt.Sprintf("%s(%q) %s: nothing embedded matches it", label, pat, verb), Details: details})
		return
	}
	if w == wantParseFS && len(dirs) > 0 {
		c.add(finding{Severity: sevError, File: fileName, Line: line, Code: "matches-dir",
			Message: fmt.Sprintf("%s(%q) fails at run time: the pattern matches the directory %s, and ParseFS can only read files", label, pat, dirs[0])})
	}
}

// globHint explains a glob that matches nothing, using what is on disk.
func (c *checker) globHint(ref *fsRef, pat string) []string {
	v := ref.v
	full := joinPrefix(ref.prefix, pat)
	for _, d := range v.dropped {
		if ok, _ := path.Match(full, d.rel); ok {
			return []string{fmt.Sprintf("%s matches it but is left out, because its name starts with %q. //go:embed all:%s includes it", d.rel, path.Base(d.rel)[:1], firstDirPattern(v, d.rel))}
		}
		if d.isDir && strings.HasPrefix(full, d.rel+"/") {
			return []string{fmt.Sprintf("it points inside %s, which is left out because its name starts with %q. //go:embed all:%s includes it", d.rel, path.Base(d.rel)[:1], firstDirPattern(v, d.rel))}
		}
	}
	diskMatches, _ := filepath.Glob(quoteGlob(v.pkg.lp.Dir) + string(filepath.Separator) + filepath.FromSlash(full))
	if len(diskMatches) > 0 {
		r, _ := filepath.Rel(v.pkg.lp.Dir, diskMatches[0])
		return []string{fmt.Sprintf("%s matches on disk, but //go:embed %s doesn't include it", filepath.ToSlash(r), strings.Join(v.patterns, " "))}
	}
	// Suggest files with the same extension.
	ext := path.Ext(pat)
	var same []string
	for _, e := range entries(ref, wantFile) {
		if ext != "" && path.Ext(e) != ext {
			continue
		}
		same = append(same, e)
	}
	if ext != "" && len(same) == 0 {
		exts := map[string]bool{}
		for _, e := range entries(ref, wantFile) {
			if x := path.Ext(e); x != "" {
				exts[x] = true
			}
		}
		if len(exts) > 0 {
			return []string{fmt.Sprintf("no embedded file ends in %s; embedded files end in %s", ext, strings.Join(uniqSorted(keys(exts)), ", "))}
		}
	}
	return nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// suggestFull suggests an embedded path close to a missing one.
func (c *checker) suggestFull(ref *fsRef, full string, w want) []string {
	var cands []string
	all := entries(&fsRef{v: ref.v, prefix: "."}, w)
	base := path.Base(full)
	for _, e := range all {
		if e != "." && path.Base(e) == base {
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		best, bestD := "", 4
		for _, e := range all {
			if e == "." {
				continue
			}
			if d := editDistance(e, full); d < bestD {
				best, bestD = e, d
			}
		}
		if best != "" {
			cands = []string{best}
		}
	}
	if len(cands) == 0 || len(cands) > 3 {
		return nil
	}
	var out []string
	for _, cand := range cands {
		shown := cand
		if ref.prefix != "." {
			if cand == ref.prefix {
				shown = "."
			} else if strings.HasPrefix(cand, ref.prefix+"/") {
				shown = cand[len(ref.prefix)+1:]
			} else {
				out = append(out, fmt.Sprintf("%s is embedded, but outside the fs.Sub(%q) directory", cand, ref.prefix))
				continue
			}
		}
		out = append(out, fmt.Sprintf("did you mean %q?", shown))
	}
	return out
}

func (c *checker) suggest(ref *fsRef, prefix string, w want) []string {
	dir := strings.TrimSuffix(prefix, "/")
	if i := strings.LastIndex(dir, "/"); i >= 0 && !strings.HasSuffix(prefix, "/") {
		dir = dir[:i]
	}
	if dir == "" || !strings.Contains(prefix, "/") {
		return nil
	}
	full := joinPrefix(ref.prefix, dir)
	if ref.v.dirs[full] {
		return nil
	}
	return []string{c.whyMissing(ref.v, full, wantDir)}
}

func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// ---------------------------------------------------------------------------
// Checking variables

// quietDropped are names that a directory pattern leaving out is what people
// want.
var quietDropped = map[string]bool{
	".gitkeep": true, ".keep": true, ".gitignore": true, ".gitattributes": true,
	".DS_Store": true, ".npmignore": true, ".prettierrc": true, ".eslintrc": true,
	".editorconfig": true, "_test": false,
}

var secretName = regexp.MustCompile(`^(\.env|\.env\..+|\.npmrc|\.netrc|\.pypirc|\.pgpass|\.htpasswd|id_rsa|id_dsa|id_ecdsa|id_ed25519|credentials\.json)$`)
var notSecret = regexp.MustCompile(`\.(example|sample|template|dist|defaults|tmpl|tpl)$`)

func (c *checker) checkVar(v *embedVar) {
	var loud []dropped
	for _, d := range v.dropped {
		if !quietDropped[path.Base(d.rel)] {
			loud = append(loud, d)
		}
	}
	if len(loud) > 0 {
		sort.Slice(loud, func(i, j int) bool { return loud[i].rel < loud[j].rel })
		var details []string
		nfiles := 0
		for i, d := range loud {
			n := 1
			if d.isDir {
				n = d.files
			}
			nfiles += n
			if i < 6 {
				if d.isDir {
					details = append(details, fmt.Sprintf("%s/ (%s)", d.rel, plural(d.files, "file", "files")))
				} else {
					details = append(details, d.rel)
				}
			}
		}
		if len(loud) > 6 {
			details = append(details, fmt.Sprintf("and %d more", len(loud)-6))
		}
		pats := map[string]bool{}
		for _, d := range loud {
			pats["all:"+firstDirPattern(v, d.rel)] = true
		}
		details = append(details, fmt.Sprintf("names starting with _ or . are skipped inside directories; write //go:embed %s to include them", strings.Join(uniqSorted(keys(pats)), " ")))
		c.add(finding{Severity: sevWarn, File: v.file, Line: v.line, Code: "dropped",
			Message: fmt.Sprintf("//go:embed %s leaves out %s on disk:", strings.Join(v.patterns, " "), plural(nfiles, "file", "files")),
			Details: details})
	}
	var secrets []string
	for f := range v.files {
		b := path.Base(f)
		if secretName.MatchString(b) && !notSecret.MatchString(b) {
			secrets = append(secrets, f)
		}
	}
	if len(secrets) > 0 {
		sort.Strings(secrets)
		c.add(finding{Severity: sevWarn, File: v.file, Line: v.line, Code: "secret",
			Message: fmt.Sprintf("//go:embed %s puts %s into the binary, where anyone who has the binary can read it:", strings.Join(v.patterns, " "), plural(len(secrets), "file that usually holds secrets", "files that usually hold secrets")),
			Details: secrets})
	}
}

// noteIgnored lists //go:embed lines in files the current build leaves out,
// because their build constraints don't match.
func (c *checker) noteIgnored() {
	var names []string
	for _, ps := range c.pkgs {
		for _, name := range ps.lp.IgnoredGoFiles {
			src, err := os.ReadFile(filepath.Join(ps.lp.Dir, name))
			if err == nil && bytes.Contains(src, []byte("//go:embed")) {
				names = append(names, c.relFile(ps, name))
			}
		}
	}
	if len(names) > 0 {
		c.findings = append(c.findings, finding{Severity: sevNote, Code: "constraints",
			Message: "these files have //go:embed lines but this GOOS, GOARCH and tag set doesn't build them, so they weren't checked:",
			Details: names})
	}
}

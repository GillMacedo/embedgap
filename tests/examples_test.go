// Package tests runs embedgap and the example programs, and checks that the
// errors embedgap reports are exactly the reads that fail when the program
// runs.
package tests

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type finding struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Code     string `json:"code"`
}

type report struct {
	Checked  int       `json:"checked"`
	Findings []finding `json:"findings"`
	Exit     int       `json:"exit"`
}

func build(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "embedgap")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "../plugin/scripts")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return exe
}

func check(t *testing.T, exe, dir string) (*report, int) {
	t.Helper()
	cmd := exec.Command(exe, "--dir", dir, "--format", "json")
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var rep report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("embedgap on %s: exit %d, bad JSON: %v\n%s", dir, code, err, out)
	}
	return &rep, code
}

// runExample runs an example program and returns the names of the checks
// that failed. Each output line is "name: ok" or "name: error".
func runExample(t *testing.T, dir string) (failed, passed map[string]bool) {
	t.Helper()
	out, _ := exec.Command("go", "run", "./"+filepath.ToSlash(dir)).Output()
	failed, passed = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, rest, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("unexpected output from %s: %q", dir, line)
		}
		if rest == "ok" {
			passed[name] = true
		} else {
			failed[name] = true
		}
	}
	if len(failed)+len(passed) == 0 {
		t.Fatalf("%s printed nothing", dir)
	}
	return failed, passed
}

// funcAt maps each line of a Go file to the function that contains it.
func funcAt(t *testing.T, file string) func(int) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func(line int) string {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fset.Position(fd.Pos()).Line <= line && line <= fset.Position(fd.End()).Line {
					return fd.Name.Name
				}
			}
		}
		return ""
	}
}

// The example's check names match its function names, except migration,
// which is called through a closure with the same name.
func TestBrokenExampleMatchesRunTime(t *testing.T) {
	exe := build(t)
	dir := filepath.Join("..", "examples", "brokenembed")
	rep, code := check(t, exe, dir)
	if code != 1 || rep.Exit != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	failed, passed := runExample(t, "../examples/brokenembed")
	at := funcAt(t, filepath.Join(dir, "main.go"))
	flagged := map[string]bool{}
	for _, f := range rep.Findings {
		if f.Severity != "ERROR" {
			continue
		}
		fn := at(f.Line)
		flagged[fn] = true
		if !failed[fn] {
			t.Errorf("embedgap reports an error in %s (main.go:%d), but it works at run time", fn, f.Line)
		}
	}
	for fn := range failed {
		if !flagged[fn] {
			t.Errorf("%s fails at run time, and embedgap reports no error in it", fn)
		}
	}
	if !passed["settings"] {
		t.Errorf("settings should work: %v", passed)
	}
	// index uses filepath.Join, which works on this platform unless it uses
	// backslashes. embedgap warns about it everywhere.
	sep := string(filepath.Separator)
	if sep == "/" && !passed["index"] || sep == `\` && !failed["index"] {
		t.Errorf("index on %s: passed=%v failed=%v", runtime.GOOS, passed, failed)
	}
	var warns []string
	for _, f := range rep.Findings {
		if f.Severity == "WARN" {
			warns = append(warns, f.Code)
		}
	}
	sort.Strings(warns)
	if strings.Join(warns, ",") != "dropped,filepath-join,secret" {
		t.Errorf("warnings: %v", warns)
	}
}

func TestFixedExampleIsQuiet(t *testing.T) {
	exe := build(t)
	rep, code := check(t, exe, filepath.Join("..", "examples", "fixedembed"))
	if code != 0 || len(rep.Findings) != 0 {
		t.Errorf("exit %d, findings %v", code, rep.Findings)
	}
	if rep.Checked == 0 {
		t.Error("checked nothing")
	}
	failed, _ := runExample(t, "../examples/fixedembed")
	if len(failed) > 0 {
		t.Errorf("fixed example fails at run time: %v", failed)
	}
}

func TestReadmeExampleCommands(t *testing.T) {
	// The README runs embedgap from the repository root with go run.
	if _, err := os.Stat(filepath.Join("..", "README.md")); err != nil {
		t.Skip("no README")
	}
	cmd := exec.Command("go", "run", "./plugin/scripts", "--dir", "examples/fixedembed")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "No problems found") {
		t.Errorf("%v\n%s", err, out)
	}
}

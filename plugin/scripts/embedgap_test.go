package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test binary doubles as a fake go command: with EMBEDGAP_FAKE_GO set,
// it prints broken go list output instead of running the tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("EMBEDGAP_FAKE_GO"); mode != "" {
		os.Exit(fakeGo(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func realList(args []string) []byte {
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "real go list failed:", err)
		os.Exit(1)
	}
	return out
}

func fakeGo(mode string, args []string) int {
	switch mode {
	case "empty":
		return 0
	case "garbage":
		fmt.Println("{{{ this is not json")
		return 0
	case "fail":
		fmt.Fprintln(os.Stderr, "go: build failed")
		return 1
	case "nofields":
		fmt.Println("{}")
		return 0
	case "truncated":
		out := realList(args)
		os.Stdout.Write(out[:len(out)/2])
		return 0
	case "trailing":
		os.Stdout.Write(realList(args))
		fmt.Println("[1,2")
		return 0
	case "fail-after-output":
		os.Stdout.Write(realList(args))
		fmt.Fprintln(os.Stderr, "go: something went wrong")
		return 1
	case "drop-embed-file", "add-embed-file":
		// Valid JSON that disagrees with the files on disk.
		dec := json.NewDecoder(bytes.NewReader(realList(args)))
		for {
			var m map[string]any
			if err := dec.Decode(&m); err == io.EOF {
				break
			} else if err != nil {
				return 1
			}
			if files, ok := m["EmbedFiles"].([]any); ok && len(files) > 0 {
				if mode == "drop-embed-file" {
					m["EmbedFiles"] = files[1:]
				} else {
					m["EmbedFiles"] = append(files, "phantom.txt")
				}
			}
			b, _ := json.Marshal(m)
			os.Stdout.Write(b)
			fmt.Println()
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown fake mode", mode)
	return 1
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if _, ok := files["go.mod"]; !ok {
		files["go.mod"] = "module example.com/m\n\ngo 1.22\n"
	}
	for name, src := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type result struct {
	code           int
	stdout, stderr string
}

func runTool(t *testing.T, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return result{code, out.String(), errb.String()}
}

func runJSON(t *testing.T, dir string) (*report, result) {
	t.Helper()
	r := runTool(t, "--dir", dir, "--format", "json")
	if r.code == exitUnusable {
		t.Fatalf("exit 2: %s", r.stderr)
	}
	var rep report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, r.stdout)
	}
	return &rep, r
}

func codes(rep *report) []string {
	var out []string
	for _, f := range rep.Findings {
		out = append(out, fmt.Sprintf("%s %s:%d", f.Code, f.File, f.Line))
	}
	return out
}

func hasCode(rep *report, code string, line int) bool {
	for _, f := range rep.Findings {
		if f.Code == code && (line == 0 || f.Line == line) {
			return true
		}
	}
	return false
}

const header = "package main\n\nimport (\n\t\"embed\"\n\t\"html/template\"\n\t\"io/fs\"\n\t\"net/http\"\n\t\"path\"\n\t\"path/filepath\"\n)\n\nvar _ = template.ParseFS\nvar _ = fs.ReadFile\nvar _ = http.FileServerFS\nvar _ = path.Join\nvar _ = filepath.Join\n\n"

// lineOf returns the 1-based line of the first line containing marker.
func lineOf(t *testing.T, src, marker string) int {
	t.Helper()
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found", marker)
	return 0
}

func TestFindings(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		files map[string]string
		want  map[string]string // code -> marker on the flagged line
		not   []string          // codes that must not appear
	}{
		{
			name:  "typo in file name",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.ReadFile(\"static/index.htm\") // M1\n}\n",
			files: map[string]string{"static/index.html": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "fs.Sub into a dropped directory",
			body:  "//go:embed web\nvar web embed.FS\n\nfunc f() {\n\tsub, _ := fs.Sub(web, \"web\")\n\tfs.ReadFile(sub, \"_app/a.js\") // M1\n}\n",
			files: map[string]string{"web/index.html": "x", "web/_app/a.js": "x"},
			want:  map[string]string{"not-embedded": "M1", "dropped": "var web"},
		},
		{
			name:  "fs.Sub with the wrong directory",
			body:  "//go:embed web\nvar web embed.FS\n\nvar sub, _ = fs.Sub(web, \"static\") // M1\n",
			files: map[string]string{"web/index.html": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "must(fs.Sub) and http.FileServerFS",
			body:  "//go:embed web\nvar web embed.FS\n\nfunc must(f fs.FS, err error) fs.FS { return f }\n\nfunc f() {\n\tsub := must(fs.Sub(web, \"web\"))\n\tfs.ReadFile(sub, \"missing.css\") // M1\n}\n",
			files: map[string]string{"web/index.html": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "ParseFS pattern matches nothing",
			body:  "//go:embed tmpl/*.html\nvar tmpl embed.FS\n\nvar t = template.Must(template.ParseFS(tmpl, \"tmpl/*.tmpl\")) // M1\n",
			files: map[string]string{"tmpl/a.html": "x"},
			want:  map[string]string{"no-match": "M1"},
		},
		{
			name:  "ParseFS pattern matches a directory",
			body:  "//go:embed tmpl\nvar tmpl embed.FS\n\nvar t, _ = template.ParseFS(tmpl, \"tmpl/*\") // M1\n",
			files: map[string]string{"tmpl/a.html": "x", "tmpl/sub/b.html": "x"},
			want:  map[string]string{"matches-dir": "M1"},
		},
		{
			name:  "method ParseFS on a template",
			body:  "//go:embed tmpl\nvar tmpl embed.FS\n\nvar t, _ = template.New(\"\").ParseFS(tmpl, \"tmpl/layout.html\") // M1\n",
			files: map[string]string{"tmpl/a.html": "x"},
			want:  map[string]string{"no-match": "M1"},
		},
		{
			name:  "fs.Glob matches nothing",
			body:  "//go:embed data\nvar data embed.FS\n\nvar m, _ = fs.Glob(data, \"data/*.json\") // M1\n",
			files: map[string]string{"data/a.yaml": "x"},
			want:  map[string]string{"no-match": "M1"},
		},
		{
			name:  "partial path that never matches",
			body:  "//go:embed migrations/*.sql\nvar mig embed.FS\n\nfunc f(n string) { mig.ReadFile(\"migration/\" + n + \".sql\") // M1\n}\n",
			files: map[string]string{"migrations/1.sql": "x"},
			want:  map[string]string{"no-match": "M1"},
		},
		{
			name:  "partial path that can match stays quiet",
			body:  "//go:embed migrations/*.sql\nvar mig embed.FS\n\nfunc f(n string) { mig.ReadFile(path.Join(\"migrations\", n+\".sql\")) }\n",
			files: map[string]string{"migrations/1.sql": "x"},
			not:   []string{"no-match", "not-embedded"},
		},
		{
			name:  "leading slash",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.ReadFile(\"/static/a.txt\") // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"invalid-path": "M1"},
		},
		{
			name:  "leading dot slash",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.Open(\"./static/a.txt\") // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"invalid-path": "M1"},
		},
		{
			name:  "backslash",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.ReadFile(\"static\\\\a.txt\") // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"invalid-path": "M1"},
		},
		{
			name:  "filepath.Join",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.ReadFile(filepath.Join(\"static\", \"a.txt\")) // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"filepath-join": "M1"},
			not:   []string{"not-embedded"},
		},
		{
			name:  "ReadFile on a directory",
			body:  "//go:embed static\nvar static embed.FS\n\nfunc f() { static.ReadFile(\"static\") // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "file embedded in another variable",
			body:  "//go:embed a.txt\nvar a embed.FS\n\n//go:embed b.txt\nvar b embed.FS\n\nfunc f() { a.ReadFile(\"b.txt\") // M1\n}\n",
			files: map[string]string{"a.txt": "x", "b.txt": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "secret pulled in by all:",
			body:  "//go:embed all:conf\nvar conf embed.FS\n",
			files: map[string]string{"conf/app.yaml": "x", "conf/.env": "x"},
			want:  map[string]string{"secret": "var conf"},
		},
		{
			name:  "env example is not a secret",
			body:  "//go:embed all:conf\nvar conf embed.FS\n",
			files: map[string]string{"conf/app.yaml": "x", "conf/.env.example": "x"},
			not:   []string{"secret"},
		},
		{
			name:  "gitkeep left out is quiet",
			body:  "//go:embed static\nvar static embed.FS\n",
			files: map[string]string{"static/a.txt": "x", "static/.gitkeep": ""},
			not:   []string{"dropped"},
		},
		{
			name:  "aliased embed import",
			body:  "//go:embed static\nvar static em.FS\n\nfunc f() { static.ReadFile(\"static/nope.txt\") // M1\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			want:  map[string]string{"not-embedded": "M1"},
		},
		{
			name:  "healthy calls stay quiet",
			body:  "//go:embed static tmpl/*.html\nvar static embed.FS\n\n//go:embed version.txt\nvar version string\n\nfunc f(n string) {\n\tstatic.ReadFile(\"static/a.txt\")\n\tstatic.ReadDir(\"static\")\n\tstatic.Open(\"static/sub\")\n\tsub, _ := fs.Sub(static, \"static\")\n\tfs.ReadFile(sub, \"sub/b.txt\")\n\tfs.Stat(sub, \".\")\n\thttp.FileServerFS(sub)\n\ttemplate.ParseFS(static, \"tmpl/*.html\")\n\tfs.Glob(static, \"static/*.txt\")\n\tstatic.ReadFile(\"static/\" + n)\n}\n",
			files: map[string]string{"static/a.txt": "x", "static/sub/b.txt": "x", "tmpl/a.html": "x", "version.txt": "1"},
			not:   []string{"not-embedded", "no-match", "invalid-path", "dropped", "secret", "matches-dir", "filepath-join"},
		},
		{
			name:  "reassigned variable isn't guessed",
			body:  "//go:embed static\nvar static embed.FS\n\nvar other embed.FS\n\nfunc f(b bool) {\n\tvar x fs.FS = static\n\tif b {\n\t\tx = other\n\t}\n\tfs.ReadFile(x, \"elsewhere.txt\")\n}\n",
			files: map[string]string{"static/a.txt": "x"},
			not:   []string{"not-embedded"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := header + tc.body
			if strings.Contains(tc.body, "em.FS") {
				src = strings.Replace(src, "\t\"embed\"", "\tem \"embed\"", 1)
			}
			files := map[string]string{"main.go": src}
			for k, v := range tc.files {
				files[k] = v
			}
			dir := writeModule(t, files)
			rep, r := runJSON(t, dir)
			for code, marker := range tc.want {
				line := lineOf(t, src, marker)
				if !hasCode(rep, code, line) {
					t.Errorf("want %s at line %d, got %v", code, line, codes(rep))
				}
			}
			for _, code := range tc.not {
				if hasCode(rep, code, 0) {
					t.Errorf("unexpected %s: %v", code, codes(rep))
				}
			}
			if len(tc.want) > 0 && r.code != exitFindings {
				t.Errorf("exit %d, want 1", r.code)
			}
		})
	}
}

func TestCrossPackage(t *testing.T) {
	// The importing package uses a /v2 module path, so the package name
	// differs from the last path element.
	dir := writeModule(t, map[string]string{
		"go.mod":     "module example.com/app/v2\n\ngo 1.22\n",
		"assets.go":  "package app\n\nimport \"embed\"\n\n//go:embed db\nvar DB embed.FS\n",
		"db/a.sql":   "x",
		"cmd/x/x.go": "package main\n\nimport (\n\t\"io/fs\"\n\n\t\"example.com/app/v2\"\n)\n\nfunc main() {\n\tapp.DB.ReadFile(\"db/b.sql\")\n\tfs.ReadFile(app.DB, \"db/c.sql\")\n\tapp.DB.ReadFile(\"db/a.sql\")\n}\n",
	})
	rep, _ := runJSON(t, dir)
	if !hasCode(rep, "not-embedded", 10) || !hasCode(rep, "not-embedded", 11) {
		t.Errorf("want findings at cmd/x/x.go:10 and 11, got %v", codes(rep))
	}
	if hasCode(rep, "not-embedded", 12) {
		t.Errorf("db/a.sql is embedded: %v", codes(rep))
	}
}

func TestTestFileEmbed(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"main.go":         "package main\n\nfunc main() {}\n",
		"main_test.go":    "package main\n\nimport (\n\t\"embed\"\n\t\"testing\"\n)\n\n//go:embed testdata\nvar td embed.FS\n\nfunc TestX(t *testing.T) { td.ReadFile(\"testdata/nope.json\") }\n",
		"testdata/a.json": "{}",
	})
	rep, _ := runJSON(t, dir)
	if !hasCode(rep, "not-embedded", 11) {
		t.Errorf("want not-embedded in main_test.go:11, got %v", codes(rep))
	}
}

func TestIgnoredFilesNoted(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"main.go":    "package main\n\nimport \"embed\"\n\n//go:embed a.txt\nvar a embed.FS\n\nfunc main() { a.ReadFile(\"a.txt\") }\n",
		"other.go":   "//go:build embedgap_never_set\n\npackage main\n\nimport \"embed\"\n\n//go:embed b.txt\nvar b embed.FS\n",
		"a.txt":      "x",
		"b.txt":      "x",
		"unused.txt": "x",
	})
	rep, r := runJSON(t, dir)
	if !hasCode(rep, "constraints", 0) {
		t.Errorf("want a constraints note, got %v", codes(rep))
	}
	if r.code != exitClean {
		t.Errorf("a note alone should exit 0, got %d", r.code)
	}
}

func TestFailOn(t *testing.T) {
	warnOnly := writeModule(t, map[string]string{
		"main.go":     "package main\n\nimport \"embed\"\n\n//go:embed all:conf\nvar conf embed.FS\n\nfunc main() { conf.ReadFile(\"conf/a.yaml\") }\n",
		"conf/a.yaml": "x",
		"conf/.env":   "x",
	})
	withError := writeModule(t, map[string]string{
		"main.go": "package main\n\nimport \"embed\"\n\n//go:embed a.txt\nvar a embed.FS\n\nfunc main() { a.ReadFile(\"b.txt\") }\n",
		"a.txt":   "x",
	})
	healthy := writeModule(t, map[string]string{
		"main.go": "package main\n\nimport \"embed\"\n\n//go:embed a.txt\nvar a embed.FS\n\nfunc main() { a.ReadFile(\"a.txt\") }\n",
		"a.txt":   "x",
	})
	cases := []struct {
		dir, failOn string
		want        int
	}{
		{warnOnly, "warn", 1}, {warnOnly, "error", 0}, {warnOnly, "none", 0},
		{withError, "warn", 1}, {withError, "error", 1}, {withError, "none", 0},
		{healthy, "warn", 0}, {healthy, "error", 0}, {healthy, "none", 0},
	}
	for _, c := range cases {
		r := runTool(t, "--dir", c.dir, "--fail-on", c.failOn)
		if r.code != c.want {
			t.Errorf("%s --fail-on %s: exit %d, want %d\n%s%s", filepath.Base(c.dir), c.failOn, r.code, c.want, r.stdout, r.stderr)
		}
	}
	r := runTool(t, "--dir", healthy)
	if !strings.Contains(r.stdout, "No problems found") {
		t.Errorf("healthy module should say so:\n%s", r.stdout)
	}
	r = runTool(t, "--dir", withError, "--fail-on", "none")
	if strings.Contains(r.stdout, "No problems found") || !strings.Contains(r.stdout, "1 error.") {
		t.Errorf("--fail-on none must still report the error:\n%s", r.stdout)
	}
}

// assertUnusable checks that nothing that looks like a result is printed and
// that the run exits 2, under every --fail-on value.
func assertUnusable(t *testing.T, dir, wantMsg string) {
	t.Helper()
	for _, failOn := range []string{"warn", "error", "none"} {
		for _, format := range []string{"text", "json"} {
			r := runTool(t, "--dir", dir, "--fail-on", failOn, "--format", format)
			if r.code != exitUnusable {
				t.Errorf("--fail-on %s --format %s: exit %d, want 2\nstdout: %s\nstderr: %s", failOn, format, r.code, r.stdout, r.stderr)
			}
			if r.stdout != "" {
				t.Errorf("--fail-on %s --format %s: printed a result for input it can't trust:\n%s", failOn, format, r.stdout)
			}
			if !strings.Contains(r.stderr, "can't trust the input") || !strings.Contains(r.stderr, wantMsg) {
				t.Errorf("stderr should explain %q:\n%s", wantMsg, r.stderr)
			}
		}
	}
}

func TestUntrustedSource(t *testing.T) {
	good := "package main\n\nimport \"embed\"\n\n//go:embed a.txt\nvar a embed.FS\n\nfunc main() { a.ReadFile(\"a.txt\") }\n"
	cases := []struct {
		name  string
		files map[string]string
		msg   string
	}{
		{"empty Go file", map[string]string{"main.go": "", "a.txt": "x"}, "doesn't build"},
		{"garbage Go file", map[string]string{"main.go": "\x00\xff\xfe garbage {{{", "a.txt": "x"}, "doesn't build"},
		{"Go file cut off partway", map[string]string{"main.go": good[:len(good)-20], "a.txt": "x"}, ""},
		{"compiler output instead of Go", map[string]string{"main.go": "# example.com/m\n./main.go:8:2: undefined: x\nFAIL\n", "a.txt": "x"}, "doesn't build"},
		{"type error", map[string]string{"main.go": strings.Replace(good, "a.ReadFile(\"a.txt\")", "a.ReadFile(\"a.txt\"); undefinedName()", 1), "a.txt": "x"}, "undefined"},
		{"type error in a test file", map[string]string{"main.go": good, "main_test.go": "package main\n\nfunc init() { nope() }\n", "a.txt": "x"}, "undefined"},
		{"embed pattern matches nothing", map[string]string{"main.go": good}, "no matching files"},
		{"empty embedded directory", map[string]string{"main.go": strings.Replace(good, "a.txt\nvar", "d\nvar", 1), "d/.gitkeep": ""}, "doesn't build"},
		{"no //go:embed at all", map[string]string{"main.go": "package main\n\nfunc main() {}\n"}, "nothing to check"},
		{"garbage go.mod", map[string]string{"go.mod": "\x00 not a go.mod {{", "main.go": good, "a.txt": "x"}, "go list failed"},
		{"empty go.mod", map[string]string{"go.mod": "", "main.go": good, "a.txt": "x"}, ""},
		{"go.mod cut off", map[string]string{"go.mod": "module example.com/m\n\ngo 1.2", "main.go": good, "a.txt": "x"}, "go1.16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertUnusable(t, writeModule(t, tc.files), tc.msg)
		})
	}
	t.Run("empty directory", func(t *testing.T) {
		assertUnusable(t, t.TempDir(), "")
	})
	t.Run("missing directory", func(t *testing.T) {
		assertUnusable(t, filepath.Join(t.TempDir(), "nope"), "not a directory")
	})
	t.Run("a file instead of a directory", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "f.go")
		os.WriteFile(f, []byte(good), 0o644)
		assertUnusable(t, f, "not a directory")
	})
}

func TestUntrustedGoList(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := writeModule(t, map[string]string{
		"main.go": "package main\n\nimport \"embed\"\n\n//go:embed a.txt b.txt\nvar a embed.FS\n\nfunc main() { a.ReadFile(\"a.txt\") }\n",
		"a.txt":   "x",
		"b.txt":   "x",
	})
	// The real go command, as a baseline, is clean.
	if r := runTool(t, "--dir", dir); r.code != exitClean {
		t.Fatalf("baseline should be clean, exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	t.Setenv("EMBEDGAP_GO", exe)
	for mode, msg := range map[string]string{
		"empty":             "no packages",
		"garbage":           "isn't valid JSON",
		"fail":              "go list failed",
		"nofields":          "no import path",
		"truncated":         "isn't valid JSON",
		"trailing":          "isn't valid JSON",
		"fail-after-output": "go list failed",
		"drop-embed-file":   "disagrees with go list",
		"add-embed-file":    "disagrees with go list",
	} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("EMBEDGAP_FAKE_GO", mode)
			assertUnusable(t, dir, msg)
		})
	}
}

func TestFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--format", "xml"},
		{"--fail-on", "high"},
		{"--nope"},
		{"somedir"},
	} {
		if r := runTool(t, args...); r.code != exitUnusable || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q", args, r.code, r.stdout)
		}
	}
	if r := runTool(t, "--version"); r.code != 0 || !strings.Contains(r.stdout, version) {
		t.Errorf("--version: %d %q", r.code, r.stdout)
	}
}

func TestParseEmbedArgs(t *testing.T) {
	got, err := parseEmbedArgs(" a.txt  \"b c.txt\" `d.txt`")
	if err != nil || strings.Join(got, "|") != "a.txt|b c.txt|d.txt" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{`"open`, "`open", `"a"b`} {
		if _, err := parseEmbedArgs(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

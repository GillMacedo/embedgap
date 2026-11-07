package tests

import (
	"embed"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

//go:embed testdata/web
var web embed.FS

// embedgap says an fs.Sub with a wrong directory returns no error and gives
// a file server that answers 404. This checks that claim against net/http.
func TestWrongSubServes404(t *testing.T) {
	sub, err := fs.Sub(web, "testdata/static")
	if err != nil {
		t.Fatalf("fs.Sub returned an error: %v", err)
	}
	for _, p := range []string{"/", "/style.css", "/a/b.js"} {
		rec := httptest.NewRecorder()
		http.FileServer(http.FS(sub)).ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
	good, _ := fs.Sub(web, "testdata/web")
	rec := httptest.NewRecorder()
	http.FileServer(http.FS(good)).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("the right directory: %d, want 200", rec.Code)
	}
}

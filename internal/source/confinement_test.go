package source

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A symlink committed to the config repository is a way out of the config
// directory. That matters because file mode is meant to be pointed at a git
// checkout (the README suggests a post-receive git pull), so the contents of
// that directory are controlled by whoever can push to the config repo -- and
// the file being read is served to browsers as the stylesheet.
func TestFetchPathRefusesASymlinkOutOfTheDirectory(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SERVER-ONLY-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nav.yaml"), []byte("groups: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak.css")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	f, err := NewFile(filepath.Join(dir, "nav.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	snap, err := f.FetchPath(context.Background(), "leak.css")
	if err == nil {
		t.Errorf("FetchPath(leak.css) = %q, want a refusal: a symlink escaped %s", snap.Data, dir)
	}
}

// Gitea always reports a blob sha, but the fallback matters when it does not:
// using the content length means two documents of the same size share a
// revision, the watcher's fingerprint does not move, and the edit is never
// published. Hashing the content keeps "the revision changes exactly when the
// content does".
func TestGiteaRevisionFollowsTheContentWhenThereIsNoSHA(t *testing.T) {
	const a = "groups: []\n"
	const b = "groups: [A\n" // same length, different bytes

	serve := func(content string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":"nav.yaml","path":"nav.yaml","size":%d,"encoding":"base64","content":%q}`,
				len(content), base64.StdEncoding.EncodeToString([]byte(content)))
		}))
	}

	rev := func(content string) string {
		srv := serve(content)
		defer srv.Close()
		src, err := NewGitea(GiteaOptions{
			BaseURL: srv.URL, Owner: "acme", Repo: "nav",
			Ref: "main", Path: "nav.yaml", Token: "tok",
		})
		if err != nil {
			t.Fatalf("NewGitea() error = %v", err)
		}
		snap, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if string(snap.Data) != content {
			t.Fatalf("Data = %q, want %q", snap.Data, content)
		}
		return snap.Rev
	}

	if len(a) != len(b) {
		t.Fatalf("the fixture must be the same length: %d vs %d", len(a), len(b))
	}
	revA, revB := rev(a), rev(b)
	if revA == "" || revB == "" {
		t.Fatal("a revision must never be empty")
	}
	if revA == revB {
		t.Errorf("revisions are equal (%q) for different content of the same length, "+
			"so a real edit would never be published", revA)
	}
}

package source

import (
	"context"
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

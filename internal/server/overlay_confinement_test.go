package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The override directory is only useful if it really is a boundary. Comparing
// unresolved paths let a symlink inside it serve a file from outside.
func TestOverlayRefusesASymlinkOutOfTheDirectory(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("SERVER-ONLY-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(dir, "app.js")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	base, err := fs.Sub(webRoot, "web")
	if err != nil {
		t.Fatal(err)
	}
	o := overlayFS{dir: dir, base: base}
	if got, ok := o.resolve("app.js"); ok {
		t.Errorf("resolve(app.js) = %q, ok; want the symlink to be refused", got)
	}
	if f, err := o.Open("app.js"); err == nil {
		_ = f.Close()
		t.Error("Open(app.js) succeeded through a symlink out of the override directory")
	}
}

// A real override must still win, including when the directory itself is
// reached through a symlink -- macOS /tmp is one, and rejecting that would break
// every deployment that puts --web-dir under it.
func TestOverlayStillServesARealOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("// override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := fs.Sub(webRoot, "web")
	if err != nil {
		t.Fatal(err)
	}
	o := overlayFS{dir: dir, base: base}

	got, ok := o.resolve("app.js")
	if !ok {
		t.Fatal("resolve(app.js) = false, want the override to be found")
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "// override\n" {
		t.Errorf("read %q, want the override contents", data)
	}
}

package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File reads nav.yaml from the local filesystem.
//
// Use it when something else already puts the repository on the box: a cron
// `git pull`, a CI deploy step, an rsync, or a checkout that ghome shares with
// other tooling. The revision is a hash of the content, so mtime games cannot
// fool it.
type File struct {
	path string
	// dir bounds every sibling read. Files referenced from the config are
	// resolved inside it and nowhere else.
	dir string
}

// NewFile returns a source reading the given path. The path is resolved to an
// absolute one up front so a later working-directory change cannot silently
// point it somewhere else.
func NewFile(path string) (*File, error) {
	if path == "" {
		return nil, fmt.Errorf("file path is required (--file)")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", path, err)
	}
	return &File{path: abs, dir: filepath.Dir(abs)}, nil
}

// Path is the absolute path being watched.
func (f *File) Path() string { return f.path }

func (f *File) Describe() string { return "file:" + f.path }

func (f *File) Fetch(ctx context.Context) (Snapshot, error) {
	return f.read(ctx, f.path)
}

// FetchPath reads a sibling file, such as the custom stylesheet referenced by
// style.customCss. The path is resolved inside the config file's directory and
// is rejected if it tries to leave.
func (f *File) FetchPath(ctx context.Context, path string) (Snapshot, error) {
	if !SafeRelativePath(path) {
		return Snapshot{}, fmt.Errorf("file: %q is not a path inside the config directory", path)
	}
	joined := filepath.Join(f.dir, filepath.FromSlash(path))

	// Belt and braces after the syntactic check: confirm the resolved path
	// really is inside the directory.
	//
	// Both sides have to be resolved before they are compared. Comparing an
	// unresolved path against the directory would be wrong in both directions:
	// a link committed to the config repository would read out of the directory
	// unnoticed, and a directory that is itself reached through a link (macOS
	// /tmp, for one) would look like an escape when it is not.
	realDir, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("file: %w", err)
	}
	real, err := filepath.EvalSymlinks(joined)
	if err != nil {
		// A missing file lands here too, which is fine: it cannot be read
		// either way and the error still names what was asked for.
		return Snapshot{}, fmt.Errorf("file: %q: %w", path, err)
	}
	if rel, err := filepath.Rel(realDir, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Snapshot{}, fmt.Errorf("file: %q resolves outside the config directory", path)
	}
	return f.read(ctx, real)
}

func (f *File) read(ctx context.Context, path string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return Snapshot{
		Data: data,
		Rev:  "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

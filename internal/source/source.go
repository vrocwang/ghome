// Package source loads nav.yaml from wherever it actually lives: a private
// Gitea/Forgejo repository over the API, or a file on the local disk.
package source

import (
	"context"
	"strings"
)

// Snapshot is one revision of the document.
//
// Rev is an opaque change marker (a git blob SHA for Gitea, a content hash for
// files). The watcher compares it to skip re-parsing unchanged content.
type Snapshot struct {
	Data []byte
	Rev  string
}

// Source fetches the navigation document. Implementations must be safe for
// concurrent use.
type Source interface {
	Fetch(ctx context.Context) (Snapshot, error)
	// Describe returns a short human readable identifier used in logs and in
	// the /healthz payload. It must never contain credentials.
	Describe() string
}

// MultiFile is implemented by sources that can also read sibling files from
// the same repository. That is what lets style.customCss live next to nav.yaml
// and ride the same push-to-update path.
type MultiFile interface {
	Source
	// FetchPath reads one file, given a repository-relative path (for the forge
	// source) or a path inside the config file's directory (for the file
	// source). Implementations must refuse to escape the repository or the
	// directory respectively.
	FetchPath(ctx context.Context, path string) (Snapshot, error)
	// NavPath names the configured document: repository-relative for the forge
	// source, absolute for the file source. It is for logs -- "which file did
	// this reload pick up" -- and never for addressing a file.
	NavPath() string
}

// SafeRelativePath rejects absolute paths and parent-directory traversal, so a
// path that arrives from a config file cannot be used to read arbitrary files.
func SafeRelativePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return false
	}
	if strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return false
	}
	// Reject Windows-style drive letters and URL schemes, in case a path is
	// copied from a browser address bar.
	if strings.Contains(p, ":") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

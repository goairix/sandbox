package etcd

import (
	"archive/tar"
	"strings"
)

// A single path and component ends represent the active producer DFS stack.
// Entries from an exited subtree cannot regain a parent on this stack.
type taskWorkspaceSyncLocalArchiveOrder struct {
	path  string
	ends  [128]uint16
	depth int
}

func (o *taskWorkspaceSyncLocalArchiveOrder) accept(e *workspaceSyncArchiveEntry) error {
	if e == nil || !validWorkspaceSyncPath(e.path) {
		return ErrInvalidRecord
	}
	parent := ""
	if i := strings.LastIndexByte(e.path, '/'); i >= 0 {
		parent = e.path[:i]
	}
	for o.depth > 0 && o.path[:o.ends[o.depth-1]] != parent {
		o.depth--
	}
	if parent != "" && o.depth == 0 {
		return ErrCorruptRecord
	}
	if e.kind == tar.TypeDir {
		if o.depth == len(o.ends) {
			return ErrInvalidRecord
		}
		o.path = e.path
		o.ends[o.depth] = uint16(len(e.path))
		o.depth++
	}
	return nil
}

package etcd

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

const (
	maxNamespaceRootBytes      = 512
	maxNamespaceKeyBytes       = 1024
	maxNamespaceComponentBytes = 128
)

// Namespace identifies one authority's cell beneath an operator-owned prefix.
// Its components are immutable and all keys are constructed without path cleaning.
// Roots are limited to 512 bytes, scope and cell to 128 bytes each, and keys to
// 1024 bytes including the root.
type Namespace struct{ prefix, scope, cell string }

// NewNamespace validates an absolute prefix and single-component scope and cell.
// Scope and cell may each use at most 128 bytes; the resulting root, including
// its trailing slash, may use at most 512 bytes.
func NewNamespace(prefix, scope, cell string) (Namespace, error) {
	if len(prefix) > maxNamespaceRootBytes {
		return Namespace{}, fmt.Errorf("%w: namespace prefix exceeds root limit", ErrInvalidConfiguration)
	}
	if !strings.HasPrefix(prefix, "/") || prefix == "/" {
		return Namespace{}, fmt.Errorf("%w: prefix must be an absolute non-root path", ErrInvalidConfiguration)
	}
	for _, s := range strings.Split(prefix[1:], "/") {
		if !validSegment(s) {
			return Namespace{}, fmt.Errorf("%w: invalid prefix component", ErrInvalidConfiguration)
		}
	}
	if !validSegment(scope) || !validSegment(cell) || len(scope) > maxNamespaceComponentBytes || len(cell) > maxNamespaceComponentBytes {
		return Namespace{}, fmt.Errorf("%w: invalid scope or cell", ErrInvalidConfiguration)
	}
	n := Namespace{prefix: prefix, scope: scope, cell: cell}
	if len(n.Root()) > maxNamespaceRootBytes {
		return Namespace{}, fmt.Errorf("%w: namespace root exceeds 512 bytes", ErrInvalidConfiguration)
	}
	return n, nil
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Root returns the namespace path with its trailing slash.
func (n Namespace) Root() string { return n.prefix + "/" + n.scope + "/" + n.cell + "/" }

// Key appends validated path components, rejecting keys longer than 1024 bytes.
func (n Namespace) Key(segments ...string) (string, error) {
	if _, err := NewNamespace(n.prefix, n.scope, n.cell); err != nil {
		return "", err
	}
	if len(segments) == 0 {
		return "", fmt.Errorf("%w: missing key components", ErrInvalidConfiguration)
	}
	size := len(n.Root())
	for i, s := range segments {
		if i > 0 {
			size++
		}
		if len(s) > maxNamespaceKeyBytes-size {
			return "", fmt.Errorf("%w: namespace key exceeds 1024 bytes", ErrInvalidConfiguration)
		}
		size += len(s)
		if !validSegment(s) {
			return "", fmt.Errorf("%w: invalid key component", ErrInvalidConfiguration)
		}
	}
	key := n.Root() + strings.Join(segments, "/")
	if len(key) > maxNamespaceKeyBytes {
		return "", fmt.Errorf("%w: namespace key exceeds 1024 bytes", ErrInvalidConfiguration)
	}
	return key, nil
}

// Partition maps an identity to one of the fixed 256 state partitions.
func Partition(identity []byte) uint8 { sum := sha256.Sum256(identity); return sum[0] }

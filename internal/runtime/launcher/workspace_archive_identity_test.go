package launcher

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestWorkspaceArchiveReadIdentityPolicy(t *testing.T) {
	for _, tc := range []struct {
		uid, gid uint32
		valid    bool
	}{
		{0, 1000, false}, {1000, 0, false}, {2147483648, 1000, false},
		{1000, 2147483648, false}, {4294967295, 4294967295, false},
		{1, 1, true}, {1000, 1000, true}, {2147483647, 2147483647, true},
	} {
		if got := archiveReadIdentityValid(tc.uid, tc.gid); got != tc.valid {
			t.Fatalf("uid=%d gid=%d accepted=%v", tc.uid, tc.gid, got)
		}
	}
}

func TestWorkspaceArchiveReadIdentityUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	var b *KernelBoundary
	if s, err := b.OpenWorkspaceArchiveAsUser(context.Background(), nil, 1000, 1000); s != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported identity source: %v %v", s, err)
	}
}

//go:build !linux

package launcher

import (
	"context"
	"io"
)

func (b *KernelBoundary) OpenWorkspaceArchive(ctx context.Context, observation *UserNamespaceQuiescenceObservation) (*WorkspaceArchiveSource, error) {
	return nil, ErrUnsupported
}

func (s *WorkspaceArchiveSource) writeArchive(ctx context.Context, dst io.Writer) (WorkspaceArchiveSummary, error) {
	return WorkspaceArchiveSummary{}, ErrUnsupported
}

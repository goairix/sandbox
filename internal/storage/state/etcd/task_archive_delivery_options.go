package etcd

import (
	"fmt"
	"math"
	"path/filepath"
)

// TaskWorkspaceArchiveSpoolOptions bounds the optional original archive store.
type TaskWorkspaceArchiveSpoolOptions struct {
	Directory                                                           string
	Jobs, JobBytes, TotalBytes, Entries, JobIndexBytes, TotalIndexBytes uint64
}

func (o TaskWorkspaceArchiveSpoolOptions) limits() taskArchiveLimits {
	return taskArchiveLimits{Jobs: o.Jobs, JobBytes: o.JobBytes, TotalBytes: o.TotalBytes, Entries: o.Entries, JobIndexBytes: o.JobIndexBytes, TotalIndexBytes: o.TotalIndexBytes}
}

// Validation has no filesystem or operator callback effects.
func validateTaskArchiveSpool(o *TaskWorkspaceArchiveSpoolOptions) error {
	if o == nil {
		return nil
	}
	l := o.limits()
	index, err := taskArchiveIndexBytes(l.Entries)
	if err != nil || l.Jobs == 0 || l.JobBytes < 1536 || l.JobBytes > 1<<40 || l.TotalBytes < l.JobBytes || l.JobIndexBytes < index || l.TotalIndexBytes < l.JobIndexBytes || index > uint64(math.MaxInt) || !filepath.IsAbs(o.Directory) || filepath.Clean(o.Directory) != o.Directory {
		return fmt.Errorf("%w: archive spool limits or directory", ErrInvalidConfiguration)
	}
	return nil
}

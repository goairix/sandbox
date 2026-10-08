package etcd

import "fmt"

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
	if _, err := validateTaskArchiveStore(o.Directory, o.limits()); err != nil {
		return fmt.Errorf("%w: archive spool limits or directory", ErrInvalidConfiguration)
	}
	return nil
}

//go:build !linux && !darwin

package controltarget

import "context"

func (j *Journal) readTaskArchiveLocked(context.Context) (*TaskWorkspaceArchiveRecord, error) {
	return nil, ErrJournalUnavailable
}
func (j *Journal) persistTaskArchivePendingLocked(context.Context, []byte) error {
	return ErrJournalUnavailable
}
func (j *Journal) persistTaskArchiveTerminalLocked(context.Context, []byte) error {
	return ErrJournalUnavailable
}

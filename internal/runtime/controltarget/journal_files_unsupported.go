//go:build !linux && !darwin

package controltarget

import "context"

func openJournalPlatform(context.Context, JournalOptions, bool, journalIOHook) (*Journal, error) {
	return nil, ErrInvalidConfiguration
}
func (j *Journal) persistClosedGateLocked(context.Context) error { return ErrInvalidConfiguration }

func (j *Journal) readCommandLocked(context.Context, string) (*ExecJournalRecord, error) {
	return nil, ErrInvalidConfiguration
}
func (j *Journal) persistNewCommandLocked(context.Context, ExecJournalRecord) error {
	return ErrInvalidConfiguration
}

func (j *Journal) persistActivationLocked(context.Context, []byte) error {
	return ErrInvalidConfiguration
}
func (j *Journal) persistOpenGateLocked(context.Context) error { return ErrInvalidConfiguration }
func (j *Journal) replaceCommandLocked(context.Context, ExecJournalRecord, ExecJournalRecord, bool) error {
	return ErrInvalidConfiguration
}

func (j *Journal) readTaskCloseLocked(context.Context) (*TaskDataCloseRecord, error) {
	return nil, ErrInvalidConfiguration
}
func (j *Journal) persistTaskCloseLocked(context.Context, []byte) error {
	return ErrInvalidConfiguration
}

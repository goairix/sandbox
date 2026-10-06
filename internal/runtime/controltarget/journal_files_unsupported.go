//go:build !linux && !darwin

package controltarget

import "context"

func openJournalPlatform(context.Context, JournalOptions, bool, journalIOHook) (*Journal, error) {
	return nil, ErrInvalidConfiguration
}
func (j *Journal) persistClosedGateLocked(context.Context) error { return ErrInvalidConfiguration }

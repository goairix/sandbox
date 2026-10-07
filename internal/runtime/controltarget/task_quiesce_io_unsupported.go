//go:build !linux && !darwin

package controltarget

import "context"

func (j *Journal) readTaskQuiescenceLocked(context.Context) (*TaskUserQuiescenceRecord, error) {
	return nil, ErrInvalidConfiguration
}
func (j *Journal) persistTaskQuiescencePendingLocked(context.Context, []byte) error {
	return ErrInvalidConfiguration
}

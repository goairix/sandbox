//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"os"
)

func (j *Journal) readTaskQuiescenceLocked(ctx context.Context) (*TaskUserQuiescenceRecord, error) {
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	b, err := j.files.readFileLimit(ctx, j.root, "users-quiesce.json", maxTaskQuiescenceRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if j.accountingKnown && int64(len(b)) != j.taskQuiescenceBytes {
		return nil, ErrConflict
	}
	r, err := decodeTaskQuiescence(b)
	if err != nil {
		return nil, err
	}
	if err = j.taskCloseBinding(r.Context.Current); err != nil {
		return nil, err
	}
	return &r, nil
}

// This producer accepts pending only; physical completion belongs to Task2.
func (j *Journal) persistTaskQuiescencePendingLocked(ctx context.Context, b []byte) error {
	r, err := decodeTaskQuiescence(b)
	if err != nil {
		return err
	}
	if r.State != "pending" {
		return ErrInvalidRecord
	}
	if !j.accountingKnown {
		return ErrJournalUnavailable
	}
	if j.logicalBytes+int64(len(b)) > j.maxBytes || j.contentFilesLocked()+1 > maxJournalContentFiles {
		return ErrCapacity
	}
	nonce, err := journalNonce()
	if err != nil {
		return err
	}
	if err = j.files.persistFile(ctx, j.root, "users-quiesce.json", ".users-quiesce."+nonce+".tmp", b); err != nil {
		return err
	}
	j.logicalBytes += int64(len(b)) - j.taskQuiescenceBytes
	j.taskQuiescenceBytes = int64(len(b))
	return nil
}

// Only the fixed original-handle terminal producer calls this replacement.
func (j *Journal) persistTaskQuiescenceTerminalLocked(ctx context.Context, b []byte) error {
	r, err := decodeTaskQuiescence(b)
	if err != nil {
		return err
	}
	if r.State != "users_quiesced" || !j.accountingKnown || j.taskQuiescenceBytes <= 0 {
		return ErrInvalidRecord
	}
	peak := j.logicalBytes + int64(len(b))
	if peak > j.maxBytes || peak*100 >= j.maxBytes*85 || j.contentFilesLocked()+1 > maxJournalContentFiles {
		return ErrCapacity
	}
	nonce, err := journalNonce()
	if err != nil {
		return err
	}
	if err = j.files.persistFile(ctx, j.root, "users-quiesce.json", ".users-quiesce."+nonce+".tmp", b); err != nil {
		return err
	}
	j.logicalBytes += int64(len(b)) - j.taskQuiescenceBytes
	j.taskQuiescenceBytes = int64(len(b))
	return nil
}

//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"os"
)

func (j *Journal) readTaskArchiveLocked(ctx context.Context) (*TaskWorkspaceArchiveRecord, error) {
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	b, err := j.files.readFileLimit(ctx, j.root, "workspace-archive.json", maxTaskArchiveRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if j.accountingKnown && int64(len(b)) != j.taskArchiveBytes {
		return nil, ErrConflict
	}
	r, err := decodeTaskArchive(b)
	if err != nil {
		return nil, err
	}
	if err = j.taskCloseBinding(r.Context.Current); err != nil {
		return nil, err
	}
	return &r, nil
}
func (j *Journal) persistTaskArchivePendingLocked(ctx context.Context, b []byte) error {
	r, err := decodeTaskArchive(b)
	if err != nil {
		return err
	}
	if r.State != "pending" || j.taskArchiveBytes != 0 {
		return ErrInvalidRecord
	}
	return j.persistTaskArchiveLocked(ctx, b)
}

// Private terminal replacement is reserved for the original physical producer.
func (j *Journal) persistTaskArchiveTerminalLocked(ctx context.Context, b []byte) error {
	r, err := decodeTaskArchive(b)
	if err != nil {
		return err
	}
	if r.State != "archive_complete" || j.taskArchiveBytes <= 0 {
		return ErrInvalidRecord
	}
	return j.persistTaskArchiveLocked(ctx, b)
}
func (j *Journal) persistTaskArchiveLocked(ctx context.Context, b []byte) error {
	if !j.accountingKnown {
		return ErrJournalUnavailable
	}
	peak := j.logicalBytes + int64(len(b))
	if peak > j.maxBytes || peak*100 >= j.maxBytes*85 || j.contentFilesLocked()+1 > maxJournalContentFiles {
		return ErrCapacity
	}
	nonce, err := journalNonce()
	if err != nil {
		return err
	}
	if err = j.files.persistFile(ctx, j.root, "workspace-archive.json", ".workspace-archive."+nonce+".tmp", b); err != nil {
		return err
	}
	j.logicalBytes += int64(len(b)) - j.taskArchiveBytes
	j.taskArchiveBytes = int64(len(b))
	return nil
}

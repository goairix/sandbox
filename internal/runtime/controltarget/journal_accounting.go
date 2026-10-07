//go:build linux || darwin

package controltarget

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// walkPage keeps at most 128 directory names alive. Recovery scans each level
// separately, so a parent page is never retained while scanning child pages.
func (f *journalFiles) walkPage(ctx context.Context, dir *os.File, visit func(string) error) error {
	for {
		var names []string
		var readErr error
		err := f.operation(ctx, "read-page", dir.Name(), func() error {
			names, readErr = dir.Readdirnames(128)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, name := range names {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = visit(name); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func journalBucket(name string) bool {
	return len(name) == 2 && strings.ContainsRune("0123456789abcdef", rune(name[0])) && strings.ContainsRune("0123456789abcdef", rune(name[1]))
}
func gateTempName(name string) bool {
	return strings.HasPrefix(name, ".gate.") && strings.HasSuffix(name, ".tmp") && journalUUID(strings.TrimSuffix(strings.TrimPrefix(name, ".gate."), ".tmp"))
}
func commandFileName(name string) (id string, temp bool, ok bool) {
	if len(name) == 41 && strings.HasSuffix(name, ".json") {
		id = name[:36]
		return id, false, journalUUID(id)
	}
	if len(name) == 78 && name[0] == '.' && name[37] == '.' && strings.HasSuffix(name, ".tmp") {
		id = name[1:37]
		return id, true, journalUUID(id) && journalUUID(name[38:74])
	}
	return "", false, false
}

func (j *Journal) scanJournalLocked(ctx context.Context) error {
	// The required gate was already decoded and bound before scanning. Missing
	// gate.json can never be substituted with a retained temporary manifest.
	bytes := j.manifestBytes
	var records, temps uint64
	var activationBytes, taskCloseBytes, taskQuiescenceBytes int64
	var firstQuiescence *TaskUserQuiescenceRecord
	var retainedClose *TaskDataCloseRecord
	var firstClose *TaskDataCloseRecord
	var requiresActivation bool
	if j.gate.Version == 2 {
		requiresActivation = true
	}
	add := func(n int, temp bool) error {
		bytes += int64(n)
		if temp {
			temps++
		} else {
			records++
		}
		if bytes > j.maxBytes || 1+records+temps+boolCount(activationBytes > 0)+boolCount(taskCloseBytes > 0)+boolCount(taskQuiescenceBytes > 0) > maxJournalContentFiles {
			return ErrCapacity
		}
		return nil
	}
	if err := j.files.walkPage(ctx, j.root, func(name string) error {
		switch name {
		case "gate.json", "lock", "commands":
			return nil
		}
		if name == "activation.json" || activationTempName(name) {
			b, err := j.files.readFileLimit(ctx, j.root, name, maxJournalActivationBytes)
			if err != nil {
				return err
			}
			bundle, err := decodeJournalActivation(b)
			if err != nil {
				return err
			}
			if bundle.Identity != j.gate.Identity || bundle.DataGateEpoch != j.gate.DataGateEpoch {
				return ErrIdentityMismatch
			}
			if name == "activation.json" {
				activationBytes = int64(len(b))
				bytes += activationBytes
				if bytes > j.maxBytes || 1+records+temps+1+boolCount(taskCloseBytes > 0)+boolCount(taskQuiescenceBytes > 0) > maxJournalContentFiles {
					return ErrCapacity
				}
				return nil
			}
			return add(len(b), true)
		}
		if name == "data-close.json" || taskCloseTempName(name) {
			requiresActivation = true
			b, err := j.files.readFile(ctx, j.root, name)
			if err != nil {
				return err
			}
			r, err := decodeTaskDataClose(b)
			if err != nil {
				return err
			}
			if err = j.taskCloseBinding(r.Context); err != nil {
				return err
			}
			if firstClose != nil && !sameTaskClose(*firstClose, r) {
				return ErrConflict
			}
			firstClose = &r
			if r.State == "data_closed" && j.gate.GateState != "closed" {
				return ErrInvalidRecord
			}
			if name == "data-close.json" {
				retainedClose = &r
				taskCloseBytes = int64(len(b))
				bytes += taskCloseBytes
				if bytes > j.maxBytes || 1+records+temps+boolCount(activationBytes > 0)+boolCount(taskQuiescenceBytes > 0)+1 > maxJournalContentFiles {
					return ErrCapacity
				}
				return nil
			}
			return add(len(b), true)
		}

		if name == "users-quiesce.json" || taskQuiescenceTempName(name) {
			requiresActivation = true
			b, err := j.files.readFileLimit(ctx, j.root, name, maxTaskQuiescenceRecordBytes)
			if err != nil {
				return err
			}
			r, err := decodeTaskQuiescence(b)
			if err != nil {
				return err
			}
			if err = j.taskCloseBinding(r.Context.Current); err != nil {
				return err
			}
			if j.gate.GateState != "closed" {
				return ErrInvalidRecord
			}
			if firstQuiescence != nil && !sameTaskQuiescence(*firstQuiescence, r) {
				return ErrConflict
			}
			firstQuiescence = &r
			if name == "users-quiesce.json" {
				taskQuiescenceBytes = int64(len(b))
				bytes += taskQuiescenceBytes
				if bytes > j.maxBytes || 1+records+temps+boolCount(activationBytes > 0)+boolCount(taskCloseBytes > 0)+1 > maxJournalContentFiles {
					return ErrCapacity
				}
				return nil
			}
			return add(len(b), true)
		}
		if !gateTempName(name) {
			return fmt.Errorf("%w: unknown root entry %s", ErrInvalidRecord, name)
		}
		b, err := j.files.readFile(ctx, j.root, name)
		if err != nil {
			return err
		}
		var m GateManifest
		if err = decodeGateManifest(b, &m); err != nil {
			return err
		}
		if m.Version == 2 {
			requiresActivation = true
		}
		if m.Identity != j.gate.Identity || m.DataGateEpoch != j.gate.DataGateEpoch {
			return ErrIdentityMismatch
		}
		return add(len(b), true)
	}); err != nil {
		return err
	}
	if err := j.files.walkPage(ctx, j.commands, func(name string) error {
		if !journalBucket(name) {
			return fmt.Errorf("%w: invalid bucket", ErrInvalidRecord)
		}
		dir, err := j.files.openChild(j.commands, name, true)
		if err != nil {
			return err
		}
		return dir.Close()
	}); err != nil {
		return err
	}
	// Enumerate the fixed 256 possible bucket names without keeping a list/map.
	// This cold-open cost is bounded independently of the number of records.
	for n := 0; n < 256; n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := fmt.Sprintf("%02x", n)
		bucket, err := j.files.openChild(j.commands, name, true)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = j.files.walkPage(ctx, bucket, func(name string) error {
			id, temp, ok := commandFileName(name)
			if !ok || id[:2] != bucket.Name() {
				return fmt.Errorf("%w: command filename or bucket", ErrInvalidRecord)
			}
			b, err := j.files.readFile(ctx, bucket, name)
			if err != nil {
				return err
			}
			var r ExecJournalRecord
			if err = decodeExecJournalRecord(b, &r); err != nil {
				return err
			}
			if r.Version == 2 {
				requiresActivation = true
			}
			if r.Context.CommandID != id {
				return fmt.Errorf("%w: command filename binding", ErrInvalidRecord)
			}
			if err = j.recordBinding(r); err != nil {
				return err
			}
			return add(len(b), temp)
		})
		closeErr := bucket.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if bytes > j.maxBytes {
		return ErrCapacity
	}
	if requiresActivation && activationBytes == 0 {
		return fmt.Errorf("%w: missing activation", ErrInvalidRecord)
	}
	if firstQuiescence != nil {
		if retainedClose == nil || retainedClose.State != "data_closed" || retainedClose.Context != firstQuiescence.Context.CloseDataContext || retainedClose.TicketDigest != firstQuiescence.Context.CloseDataTicketDigest {
			return ErrConflict
		}
	}
	j.taskQuiescenceBytes = taskQuiescenceBytes
	j.usersClosed = firstQuiescence != nil
	j.activationBytes = activationBytes
	j.taskCloseBytes = taskCloseBytes
	j.logicalBytes = bytes
	j.records = records
	j.temporaryFiles = temps
	j.accountingKnown = true
	return nil
}

func activationTempName(name string) bool {
	return strings.HasPrefix(name, ".activation.") && strings.HasSuffix(name, ".tmp") && journalUUID(strings.TrimSuffix(strings.TrimPrefix(name, ".activation."), ".tmp"))
}
func boolCount(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func taskCloseTempName(name string) bool {
	return strings.HasPrefix(name, ".data-close.") && strings.HasSuffix(name, ".tmp") && journalUUID(strings.TrimSuffix(strings.TrimPrefix(name, ".data-close."), ".tmp"))
}

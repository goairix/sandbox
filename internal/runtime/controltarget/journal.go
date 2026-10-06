package controltarget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
)

const (
	defaultJournalMaxBytes int64  = 256 * 1024 * 1024
	minJournalMaxBytes     int64  = 64 * 1024
	maxJournalContentFiles uint64 = 65536
)

// journalIOHook observes/injects at synchronous boundaries around real syscalls.
// A hook never replaces persistence. It is private and configured before use.
type journalIOHook func(operation, name string, after bool) error

type journalFiles struct {
	uid  uint32
	hook journalIOHook
}

// Journal is passive diagnostic storage. Its lock and pinned descriptors live
// until Close. The mutex also protects Status and all private command helpers.
type Journal struct {
	mu                                             sync.Mutex
	files                                          journalFiles
	root, commands, lock                           *os.File
	gate                                           GateManifest
	maxBytes, logicalBytes, manifestBytes          int64
	records, temporaryFiles                        uint64
	initialized, poisoned, accountingKnown, closed bool
}

func CreateClosedJournal(ctx context.Context, o JournalOptions) (*Journal, error) {
	return newJournal(ctx, o, true, nil)
}
func OpenClosedJournal(ctx context.Context, o JournalOptions) (*Journal, error) {
	return newJournal(ctx, o, false, nil)
}

func newJournal(ctx context.Context, o JournalOptions, create bool, hook journalIOHook) (*Journal, error) {
	if ctx == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = defaultJournalMaxBytes
	}
	if o.ManagementUID != uint32(os.Geteuid()) || o.MaxBytes < minJournalMaxBytes || o.MaxBytes > defaultJournalMaxBytes || o.DataGateEpoch <= 0 {
		return nil, ErrInvalidConfiguration
	}
	if err := o.Identity.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfiguration, err)
	}
	return openJournalPlatform(ctx, o, create, hook)
}

// checkLocked is shared by the point operations. Callers hold mu. A poisoned
// handle still permits validated history reads, but no persistence.
func (j *Journal) checkLocked(ctx context.Context, write bool) error {
	if ctx == nil {
		return ErrInvalidConfiguration
	}
	if j == nil || !j.initialized {
		return ErrJournalUnavailable
	}
	if j.closed {
		return ErrClosed
	}
	if write && j.poisoned {
		return ErrJournalUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (j *Journal) poison(err error) error {
	j.poisoned = true
	j.accountingKnown = false
	return fmt.Errorf("%w: %w", ErrJournalUnavailable, err)
}

func (j *Journal) CloseGate(ctx context.Context, expectedEpoch int64) (GateManifest, error) {
	if ctx == nil {
		return GateManifest{}, ErrInvalidConfiguration
	}
	if j == nil {
		return GateManifest{}, ErrJournalUnavailable
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.checkLocked(ctx, true); err != nil {
		return GateManifest{}, err
	}
	if expectedEpoch != j.gate.DataGateEpoch {
		return GateManifest{}, ErrIdentityMismatch
	}
	if err := j.persistClosedGateLocked(ctx); err != nil {
		return GateManifest{}, err
	}
	return j.gate, nil
}

func (j *Journal) Status() JournalStatus {
	if j == nil {
		return JournalStatus{Closed: true, NewRecordsStopped: true}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.initialized {
		return JournalStatus{Closed: true, NewRecordsStopped: true}
	}
	return JournalStatus{Gate: j.gate, LogicalBytes: j.logicalBytes, Records: j.records, TemporaryFiles: j.temporaryFiles, Warning: j.logicalBytes*100 >= j.maxBytes*70, NewRecordsStopped: j.closed || j.poisoned || !j.accountingKnown || j.logicalBytes*100 >= j.maxBytes*85 || 1+j.records+j.temporaryFiles+1 >= maxJournalContentFiles, Poisoned: j.poisoned, AccountingKnown: j.accountingKnown, Closed: j.closed}
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || !j.initialized {
		return nil
	}
	j.closed = true
	// Closing the actual lock descriptor releases flock, including on hard exit.
	var errs []error
	for _, f := range []*os.File{j.commands, j.lock, j.root} {
		if f != nil {
			if err := f.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	j.commands = nil
	j.lock = nil
	j.root = nil
	return errors.Join(errs...)
}

func (f *journalFiles) operation(ctx context.Context, op, name string, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.hook != nil {
		if err := f.hook(op, name, false); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := call(); err != nil {
		return err
	}
	if f.hook != nil {
		if err := f.hook(op, name, true); err != nil {
			return err
		}
	}
	return ctx.Err()
}

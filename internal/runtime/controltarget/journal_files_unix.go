//go:build linux || darwin

package controltarget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Pin every component without following aliases. Only the immediate parent is
// part of the management-owned 0700 boundary; ancestors can be system dirs.
func openJournalParent(path string, uid uint32) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("%w: canonical absolute directory required", ErrInvalidConfiguration)
	}
	parent := filepath.Dir(path)
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: parent: %w", ErrInvalidConfiguration, err)
	}
	if canonical != parent {
		return nil, fmt.Errorf("%w: parent alias", ErrInvalidConfiguration)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
		if name == "" {
			continue
		}
		next, e := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), parent)
	if err = validateJournalFD(f, uid, true); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func journalName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\x00")
}

func validateJournalFD(f *os.File, uid uint32, directory bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	mode := uint32(0600)
	kind := uint32(unix.S_IFREG)
	if directory {
		mode = 0700
		kind = unix.S_IFDIR
	}
	if st.Uid != uid || uint32(st.Mode)&unix.S_IFMT != kind || uint32(st.Mode)&07777 != mode || (!directory && st.Nlink != 1) {
		return fmt.Errorf("%w: unsafe owner, mode, type or links: %s", ErrInvalidRecord, f.Name())
	}
	return nil
}

func (f *journalFiles) openChild(dir *os.File, name string, directory bool) (*os.File, error) {
	if !journalName(name) {
		return nil, fmt.Errorf("%w: invalid child name", ErrInvalidRecord)
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(fd), name)
	if err = validateJournalFD(child, f.uid, directory); err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func (f *journalFiles) readFile(ctx context.Context, dir *os.File, name string) ([]byte, error) {
	if ctx == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	child, err := f.openChild(dir, name, false)
	if err != nil {
		return nil, err
	}
	defer child.Close()
	st, err := child.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > maxJournalWireBytes {
		return nil, fmt.Errorf("%w: oversized file", ErrInvalidRecord)
	}
	b, err := io.ReadAll(io.LimitReader(child, maxJournalWireBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxJournalWireBytes || int64(len(b)) != st.Size() {
		return nil, fmt.Errorf("%w: file size changed", ErrInvalidRecord)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return b, nil
}

func (f *journalFiles) syncDir(ctx context.Context, dir *os.File) error {
	return f.operation(ctx, "dir-sync", dir.Name(), dir.Sync)
}

func (f *journalFiles) makeDir(ctx context.Context, dir *os.File, name string) (*os.File, error) {
	if !journalName(name) {
		return nil, ErrInvalidRecord
	}
	if err := f.operation(ctx, "mkdir", name, func() error { return unix.Mkdirat(int(dir.Fd()), name, 0700) }); err != nil {
		return nil, err
	}
	child, err := f.openChild(dir, name, true)
	if err != nil {
		return nil, err
	}
	if err = f.syncDir(ctx, child); err == nil {
		err = f.syncDir(ctx, dir)
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func openJournalPlatform(ctx context.Context, o JournalOptions, create bool, hook journalIOHook) (_ *Journal, result error) {
	parent, err := openJournalParent(o.Directory, o.ManagementUID)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	j := &Journal{files: journalFiles{uid: o.ManagementUID, hook: hook}, maxBytes: o.MaxBytes, initialized: true, gate: GateManifest{Version: 1, Identity: o.Identity, DataGateEpoch: o.DataGateEpoch, GateState: "closed"}}
	defer func() {
		if result != nil {
			j.Close()
		}
	}()
	name := filepath.Base(o.Directory)
	if create {
		j.root, err = j.files.makeDir(ctx, parent, name)
	} else {
		j.root, err = j.files.openChild(parent, name, true)
	}
	if err != nil {
		return nil, err
	}
	if create {
		fd, e := unix.Openat(int(j.root.Fd()), "lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e != nil {
			return nil, e
		}
		j.lock = os.NewFile(uintptr(fd), "lock")
		if err = validateJournalFD(j.lock, o.ManagementUID, false); err != nil {
			return nil, err
		}
	} else {
		j.lock, err = j.files.openChild(j.root, "lock", false)
		if err != nil {
			return nil, err
		}
	}
	st, err := j.lock.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() != 0 {
		return nil, fmt.Errorf("%w: nonempty lock", ErrInvalidRecord)
	}
	if err = unix.Flock(int(j.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %w", ErrBusy, err)
		}
		return nil, err
	}
	if create {
		if err = j.files.operation(ctx, "file-sync", "lock", j.lock.Sync); err != nil {
			return nil, err
		}
		j.commands, err = j.files.makeDir(ctx, j.root, "commands")
		if err != nil {
			return nil, err
		}
		j.accountingKnown = true
	} else {
		j.commands, err = j.files.openChild(j.root, "commands", true)
		if err != nil {
			return nil, err
		}
		b, e := j.files.readFile(ctx, j.root, "gate.json")
		if e != nil {
			return nil, e
		}
		var gate GateManifest
		if e = decodeGateManifest(b, &gate); e != nil {
			return nil, e
		}
		if gate.Identity != o.Identity || gate.DataGateEpoch != o.DataGateEpoch {
			return nil, ErrIdentityMismatch
		}
		j.gate = gate
		j.logicalBytes = int64(len(b))
		j.manifestBytes = int64(len(b))
		if err = j.scanJournalLocked(ctx); err != nil {
			return nil, err
		}
	}
	if err = j.persistClosedGateLocked(ctx); err != nil {
		return nil, err
	}
	if create {
		if err = j.files.syncDir(ctx, parent); err != nil {
			return nil, j.poison(err)
		}
	}
	return j, nil
}

func journalNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

// persistFile leaves every uncertain byte in place. Only a fully successful
// rename and directory fsync permit the caller to publish new accounting.
func (f *journalFiles) persistFile(ctx context.Context, dir *os.File, name, temp string, b []byte) error {
	var child *os.File
	defer func() {
		if child != nil {
			child.Close()
		}
	}()
	if err := f.operation(ctx, "open-temp", temp, func() error {
		fd, err := unix.Openat(int(dir.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err == nil {
			child = os.NewFile(uintptr(fd), temp)
		}
		return err
	}); err != nil {
		return err
	}
	if err := validateJournalFD(child, f.uid, false); err != nil {
		return err
	}
	if err := f.operation(ctx, "write", temp, func() error {
		for len(b) > 0 {
			n, err := child.Write(b)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
		return nil
	}); err != nil {
		return err
	}
	if err := f.operation(ctx, "file-sync", temp, child.Sync); err != nil {
		return err
	}
	if err := f.operation(ctx, "rename", name, func() error { return unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name) }); err != nil {
		return err
	}
	return f.syncDir(ctx, dir)
}

func (j *Journal) persistClosedGateLocked(ctx context.Context) error {
	gate := j.gate
	gate.GateState = "closed"
	b, err := encodeGateManifest(gate)
	if err != nil {
		return err
	}
	count := j.records + j.temporaryFiles
	if j.manifestBytes > 0 {
		count++
	}
	if j.logicalBytes+int64(len(b)) > j.maxBytes || count+1 > maxJournalContentFiles {
		return ErrCapacity
	}
	nonce, err := journalNonce()
	if err != nil {
		return err
	}
	if err = j.files.persistFile(ctx, j.root, "gate.json", ".gate."+nonce+".tmp", b); err != nil {
		return j.poison(err)
	}
	j.logicalBytes += int64(len(b)) - j.manifestBytes
	j.manifestBytes = int64(len(b))
	j.gate = gate
	return nil
}

// readCommandLocked is a fixed-depth, verified disk read, including when
// poisoned. Its caller holds mu. Absence is nil,nil; it returns an owned value.
func (j *Journal) readCommandLocked(ctx context.Context, id string) (*ExecJournalRecord, error) {
	if err := j.checkLocked(ctx, false); err != nil {
		return nil, err
	}
	if !journalUUID(id) {
		return nil, ErrInvalidRecord
	}
	bucket, err := j.files.openChild(j.commands, id[:2], true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer bucket.Close()
	b, err := j.files.readFile(ctx, bucket, id+".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r ExecJournalRecord
	if err = decodeExecJournalRecord(b, &r); err != nil {
		return nil, err
	}
	if r.Context.CommandID != id {
		return nil, ErrInvalidRecord
	}
	if err = j.recordBinding(r); err != nil {
		return nil, err
	}
	return &r, nil
}

// persistNewCommandLocked never replaces an existing command. The evidence
// consumer handles canonical same-record retries before invoking this helper.
// Its caller holds mu. Any uncertainty after directory/file mutation poisons.
func (j *Journal) persistNewCommandLocked(ctx context.Context, r ExecJournalRecord) error {
	if err := j.checkLocked(ctx, true); err != nil {
		return err
	}
	b, err := encodeExecJournalRecord(r)
	if err != nil {
		return err
	}
	if err = j.recordBinding(r); err != nil {
		return err
	}
	existing, err := j.readCommandLocked(ctx, r.Context.CommandID)
	if err != nil {
		return err
	}
	if existing != nil {
		return ErrConflict
	}
	if !j.accountingKnown {
		return ErrJournalUnavailable
	}
	if (j.logicalBytes+int64(len(b)))*100 >= j.maxBytes*85 || 1+j.records+j.temporaryFiles+1 >= maxJournalContentFiles {
		return ErrCapacity
	}
	nonce, err := journalNonce()
	if err != nil {
		return err
	}
	bucket, err := j.files.openChild(j.commands, r.Context.CommandID[:2], true)
	if errors.Is(err, os.ErrNotExist) {
		bucket, err = j.files.makeDir(ctx, j.commands, r.Context.CommandID[:2])
		if err != nil {
			return j.poison(err)
		}
	} else if err != nil {
		return err
	}
	defer bucket.Close()
	if err = j.files.persistFile(ctx, bucket, r.Context.CommandID+".json", "."+r.Context.CommandID+"."+nonce+".tmp", b); err != nil {
		return j.poison(err)
	}
	j.records++
	j.logicalBytes += int64(len(b))
	return nil
}

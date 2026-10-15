package nodeobserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const maxJournalSlotBytes = 32768

// All embedded wires are retained verbatim as JSON, avoiding base64 backing
// allocations. Unknown attempts retain their slot permanently in this producer.
type journalRecord struct {
	Version    uint32          `json:"version"`
	State      string          `json:"state"`
	Enrollment json.RawMessage `json:"enrollment"`
	Intent     json.RawMessage `json:"intent,omitempty"`
	Ticket     json.RawMessage `json:"ticket,omitempty"`
	Terminal   json.RawMessage `json:"terminal,omitempty"`
}

type journalGeneration struct {
	Sequence uint64        `json:"sequence"`
	Record   journalRecord `json:"record"`
	SHA256   string        `json:"sha256"`
}

type journal struct {
	mu       sync.Mutex
	root     *os.Root
	dir      *os.File
	records  []journalRecord
	sequence []uint64
	failed   error
	closed   bool
}

func slotName(slot uint32, generation uint64) string {
	suffix := "a"
	if generation%2 == 0 {
		suffix = "b"
	}
	return fmt.Sprintf("slot-%04d.%s", slot, suffix)
}

func journalPayload(r journalRecord) ([]byte, error) {
	if r.Version != 1 || len(r.Enrollment) == 0 || !json.Valid(r.Enrollment) {
		return nil, fmt.Errorf("invalid observer journal enrollment")
	}
	switch r.State {
	case "enrolled":
		if len(r.Intent)+len(r.Ticket)+len(r.Terminal) != 0 {
			return nil, fmt.Errorf("enrolled journal carries command")
		}
	case "accepted", "signal-issued", "terminal":
		if len(r.Intent) == 0 || len(r.Ticket) == 0 || !json.Valid(r.Intent) || !json.Valid(r.Ticket) || (r.State == "terminal") != (len(r.Terminal) > 0) {
			return nil, fmt.Errorf("invalid observer journal command state")
		}
	default:
		return nil, fmt.Errorf("invalid observer journal state")
	}
	if len(r.Enrollment) > 8192 || len(r.Intent) > 16384 || len(r.Ticket) > 8192 || len(r.Terminal) > 4096 {
		return nil, fmt.Errorf("observer journal wire limit exceeded")
	}
	p, err := json.Marshal(r)
	if err != nil || len(p) > maxJournalSlotBytes-128 {
		return nil, fmt.Errorf("observer journal combined state exceeds slot")
	}
	return p, nil
}

func openJournal(path string, slots uint32) (_ *journal, result error) {
	if slots == 0 || slots > 1024 || !filepath.IsAbs(path) {
		return nil, fmt.Errorf("invalid observer journal configuration")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("observer journal requires private existing directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	j := &journal{root: root, dir: dir, records: make([]journalRecord, slots), sequence: make([]uint64, slots)}
	defer func() {
		if result != nil {
			j.close()
		}
	}()
	names, err := dir.Readdirnames(int(2*slots + 2))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	allowed := make(map[string]bool, 3*slots+1)
	allowed["observer.lock"] = true
	for slot := uint32(0); slot < slots; slot++ {
		allowed[slotName(slot, 1)], allowed[slotName(slot, 2)] = true, true
		allowed[fmt.Sprintf("next-%04d", slot)] = true
	}
	if len(names) > int(2*slots+1) {
		return nil, fmt.Errorf("observer journal directory exceeds fixed budget")
	}
	for _, name := range names {
		if !allowed[name] {
			return nil, fmt.Errorf("unknown or out-of-range observer journal file")
		}
	}
	for slot := uint32(0); slot < slots; slot++ {
		payloads := 0
		for _, name := range names {
			if name == slotName(slot, 1) || name == slotName(slot, 2) || name == fmt.Sprintf("next-%04d", slot) {
				payloads++
			}
		}
		if payloads > 2 {
			return nil, fmt.Errorf("observer slot exceeds two-generation budget")
		}
	}
	for slot := uint32(0); slot < slots; slot++ {
		for _, generation := range []uint64{1, 2} {
			name := slotName(slot, generation)
			stat, err := root.Lstat(name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() > maxJournalSlotBytes {
				return nil, fmt.Errorf("invalid observer journal generation %s", name)
			}
			f, err := root.Open(name)
			if err != nil {
				return nil, err
			}
			wire, readErr := io.ReadAll(io.LimitReader(f, maxJournalSlotBytes+1))
			if err = errors.Join(readErr, f.Close()); err != nil {
				return nil, err
			}
			var g journalGeneration
			if err = json.Unmarshal(wire, &g); err != nil || g.Sequence == 0 || slotName(slot, g.Sequence) != name {
				return nil, fmt.Errorf("corrupt observer journal generation %s", name)
			}
			payload, err := journalPayload(g.Record)
			canonical, marshalErr := json.Marshal(g)
			sum := sha256.Sum256(payload)
			if err != nil || marshalErr != nil || !bytes.Equal(canonical, wire) || g.SHA256 != hex.EncodeToString(sum[:]) {
				return nil, fmt.Errorf("corrupt observer journal payload %s", name)
			}
			if j.sequence[slot] > 0 && (g.Sequence+1 < j.sequence[slot] || j.sequence[slot]+1 < g.Sequence) {
				return nil, fmt.Errorf("nonconsecutive observer journal generations")
			}
			if g.Sequence > j.sequence[slot] {
				j.records[slot], j.sequence[slot] = g.Record, g.Sequence
			}
		}
		// An interrupted unpublished temporary generation never authorizes an
		// effect. Removing this fixed owned name keeps the two-payload bound.
		if err := root.Remove(fmt.Sprintf("next-%04d", slot)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if err := dir.Sync(); err != nil {
		return nil, err
	}
	return j, nil
}

func (j *journal) reserve(r journalRecord) (uint32, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r.State != "enrolled" {
		return 0, fmt.Errorf("observer slot must start enrolled")
	}
	for i, sequence := range j.sequence {
		if sequence == 0 {
			return uint32(i), j.putLocked(uint32(i), r)
		}
	}
	return 0, fmt.Errorf("observer journal full")
}

func (j *journal) put(slot uint32, r journalRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.putLocked(slot, r)
}

func (j *journal) putLocked(slot uint32, r journalRecord) error {
	if j.closed || j.failed != nil || slot >= uint32(len(j.records)) {
		return fmt.Errorf("observer journal unavailable: %v", j.failed)
	}
	payload, err := journalPayload(r)
	if err != nil {
		return err
	}
	old := j.records[slot]
	if j.sequence[slot] > 0 {
		allowed := (old.State == "enrolled" && r.State == "accepted") || (old.State == "accepted" && r.State == "signal-issued") || (old.State == "signal-issued" && r.State == "terminal")
		if !allowed || !bytes.Equal(old.Enrollment, r.Enrollment) || (len(old.Intent) > 0 && (!bytes.Equal(old.Intent, r.Intent) || !bytes.Equal(old.Ticket, r.Ticket))) {
			return fmt.Errorf("observer journal transition refused")
		}
	}
	sequence := j.sequence[slot] + 1
	sum := sha256.Sum256(payload)
	g := journalGeneration{Sequence: sequence, Record: r, SHA256: hex.EncodeToString(sum[:])}
	wire, err := json.Marshal(g)
	if err != nil || len(wire) > maxJournalSlotBytes {
		return fmt.Errorf("observer journal generation exceeds slot")
	}
	target := slotName(slot, sequence)
	temporary := fmt.Sprintf("next-%04d", slot)
	// Delete the inactive generation before creating its replacement, so no
	// crash point can retain more than two slot payloads.
	if err = j.root.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		j.failed = err
		return err
	}
	if err = j.dir.Sync(); err != nil {
		j.failed = err
		return err
	}
	f, err := j.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		j.failed = err
		return err
	}
	n, writeErr := f.Write(wire)
	if writeErr == nil && n != len(wire) {
		writeErr = io.ErrShortWrite
	}
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err == nil {
		err = j.root.Rename(temporary, target)
	}
	if err == nil {
		err = j.dir.Sync()
	}
	if err != nil {
		j.failed = err
		return err
	}
	var copied journalRecord
	if err = json.Unmarshal(payload, &copied); err != nil {
		j.failed = err
		return err
	}
	j.records[slot], j.sequence[slot] = copied, sequence
	return nil
}

func (j *journal) close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	err := errors.Join(j.dir.Close(), j.root.Close())
	j.records, j.sequence = nil, nil
	return err
}

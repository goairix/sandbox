package controltarget

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
)

func (i JournalIdentity) Validate() error {
	if !journalNamespace(i.Namespace) || !journalOpaque(i.AuthorityID) || !journalOpaque(i.Target) || !journalID(i.RestoreEpoch) || !journalID(i.SandboxID) || !journalHash(i.WorkspaceHash) || i.Generation <= 0 || !journalRuntime(i.Runtime) {
		return fmt.Errorf("%w: identity shape", ErrInvalidRecord)
	}
	return nil
}

// Validate permits historical open manifests. Only the closed state may be
// produced by the passive encoder; neither decoded state grants launch authority.
func (m GateManifest) Validate() error {
	if err := m.Identity.Validate(); err != nil {
		return err
	}
	if (m.Version != 1 && m.Version != 2) || m.DataGateEpoch <= 0 || (m.GateState != "open" && m.GateState != "closed") {
		return fmt.Errorf("%w: gate manifest shape", ErrInvalidRecord)
	}
	return nil
}

func (r ExecJournalRecord) Validate() error {
	c := r.Context
	i := JournalIdentity{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}
	if err := i.Validate(); err != nil {
		return err
	}
	if (r.Version != 1 && r.Version != 2) || !journalUUID(c.IssuerCertificateID) || !journalHash(c.IssuerCertificateDigest) || !journalUUID(c.CommandID) || !journalUUID(c.OperationID) || !journalID(c.RequestID) || !journalHash(c.OperationDigest) || c.DataGateEpoch <= 0 || c.ControlRevision <= 0 || c.AdmissionRevision <= 0 || c.LeaseID <= 0 || !journalUTC(c.ExpiresAt) || !journalHash(r.DescriptorDigest) || !journalHash(r.TicketDigest) {
		return fmt.Errorf("%w: command record shape", ErrInvalidRecord)
	}
	if !journalUTC(r.NotBefore) || !journalUTC(r.NotAfter) || !r.NotBefore.Before(r.NotAfter) || r.NotAfter.Sub(r.NotBefore) > 30*time.Second || r.NotAfter.After(c.ExpiresAt) {
		return fmt.Errorf("%w: command interval", ErrInvalidRecord)
	}
	if r.Version == 1 {
		if r.State != "unknown" || !r.AuthorityDeadline.IsZero() || r.RootPID != 0 || r.RootWaitStatus != 0 || r.DrainConfirmed || r.Reason != "" {
			return fmt.Errorf("%w: version1 result fields", ErrInvalidRecord)
		}
		return nil
	}
	if !journalUTC(r.AuthorityDeadline) || r.AuthorityDeadline.Before(r.NotAfter) || r.AuthorityDeadline.After(c.ExpiresAt) {
		return fmt.Errorf("%w: authority deadline", ErrInvalidRecord)
	}
	switch r.State {
	case "accepted":
		if r.RootPID != 0 || r.RootWaitStatus != 0 || r.DrainConfirmed || r.Reason != "" {
			return ErrInvalidRecord
		}
	case "unknown":
		if r.RootPID != 0 || r.RootWaitStatus != 0 || r.DrainConfirmed || !journalOpaque(r.Reason) {
			return ErrInvalidRecord
		}
	case "local_terminal":
		s := r.RootWaitStatus
		exited := s <= 0xffff && s&0xff == 0
		signaled := s&0xffffff00 == 0 && s&0x7f > 0 && s&0x7f < 0x7f
		if r.RootPID < 1 || r.RootPID > 2147483647 || !r.DrainConfirmed || (!exited && !signaled) || !journalOpaque(r.Reason) {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func journalOpaque(s string) bool {
	if len(s) == 0 || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func journalRuntime(r controlprotocol.RuntimeReference) bool {
	return journalOpaque(r.ID) && journalOpaque(r.UID) && journalOpaque(r.BootID)
}
func journalID(s string) bool { return len(s) <= 128 && journalSegment(s) }
func journalSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range []byte(s) {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func journalNamespace(s string) bool {
	if len(s) < 2 || len(s) > 512 || !strings.HasPrefix(s, "/") || !strings.HasSuffix(s, "/") {
		return false
	}
	parts := strings.Split(s[1:len(s)-1], "/")
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		if !journalSegment(p) {
			return false
		}
	}
	return len(parts[len(parts)-2]) <= 128 && len(parts[len(parts)-1]) <= 128
}
func journalHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
func journalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
func journalUTC(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Year() >= 0 && t.Year() <= 9999
}

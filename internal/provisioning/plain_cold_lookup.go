package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
	"unicode/utf8"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
)

var ErrPlainColdNotFound = errors.New("plain cold sandbox not found")
var ErrPlainColdUnavailable = errors.New("plain cold sandbox unavailable")

// PlainColdLookupResult is an owned current observation. It cannot authorize
// execution, renew a TTL, or promise future runtime availability.
type PlainColdLookupResult struct {
	RequestID, IntentID, SandboxID string
	WorkspaceHash                  string
	Generation                     int64
	Runtime                        p.RuntimeReference
	CreatedAt, ExpiresAt           time.Time
	Business                       *sandboxsnapshot.Snapshot
}

func checkPlainColdLookupTTL(now, expiry time.Time) error {
	if now.IsZero() || now.Location() != time.UTC || expiry.IsZero() || expiry.Location() != time.UTC || !now.Add(time.Second).Before(expiry) {
		return ErrPlainColdUnavailable
	}
	return nil
}

func decodeColdDispatch(wire []byte) (plainColdDispatch, error) {
	var d plainColdDispatch
	if len(wire) == 0 || len(wire) > 65536 || !utf8.Valid(wire) {
		return d, ErrPlainColdProfile
	}
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return plainColdDispatch{}, ErrPlainColdProfile
	}
	canonical, err := json.Marshal(d)
	if err != nil || !bytes.Equal(canonical, wire) || d.Version != 1 || d.CreatedAt.IsZero() || d.CreatedAt.Location() != time.UTC || d.CreatedAt.Year() < 1 || d.CreatedAt.Year() > 9999 {
		return plainColdDispatch{}, ErrPlainColdProfile
	}
	return d, nil
}

type plainColdLoadedTarget struct {
	view     *state.SandboxReadView
	control  *state.SandboxControlRecord
	capsule  plainColdCapsule
	dispatch plainColdDispatch
	history  *state.SandboxRuntimeReadResult
	business *sandboxsnapshot.Snapshot
	// snapshot retains the same reader-owned immutable envelope as view, so
	// destroy profile admission never relies on a second, unrelated read.
	snapshot *state.SandboxSnapshotRecord
}

// loadColdTarget reads the original fixed deployment profile and retained
// target history. Its result is private metadata, never execution authority.
func (s *PlainColdService) loadColdTarget(ctx context.Context, sandboxID string) (*plainColdLoadedTarget, error) {
	read, err := s.backend.ReadSandboxSnapshot(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if read == nil {
		return nil, ErrPlainColdNotFound
	}
	if read.View == nil || read.Snapshot == nil {
		return nil, state.ErrCorruptRecord
	}
	current := read.View.Records()
	c := current.Control
	if c == nil || c.Runtime == nil || c.Phase != state.PhaseActive || current.Owner == nil || current.Owner.Runtime == nil || *current.Owner.Runtime != *c.Runtime {
		return nil, ErrPlainColdUnavailable
	}
	var capsule plainColdCapsule
	if err := decodeColdCapsule(read.Snapshot.Payload, &capsule); err != nil {
		return nil, err
	}
	if capsule.Deployment != s.profile || capsule.SandboxID != sandboxID || capsule.SandboxID != c.SandboxID || capsule.IntentID != c.IntentID || capsule.WorkspaceHash != c.WorkspaceHash || capsule.Partition != current.Placement.Partition || capsule.SnapshotVersion != c.Snapshot.Version {
		return nil, ErrPlainColdProfile
	}
	if err := s.validateColdBusiness(capsule); err != nil {
		return nil, err
	}
	history, err := s.backend.ReadSandboxRuntime(ctx, read.View)
	if err != nil {
		return nil, err
	}
	if history == nil || history.Publication == nil || history.Dispatch == nil {
		return nil, state.ErrCorruptRecord
	}
	dispatch, err := decodeColdDispatch(history.Dispatch.Payload)
	if err != nil {
		return nil, err
	}
	originalCapsule, err := json.Marshal(dispatch.Capsule)
	if err != nil || !bytes.Equal(originalCapsule, read.Snapshot.Payload) || !dispatch.CreatedAt.Add(capsule.TTL).Equal(history.Dispatch.Record.ExpiresAt) || history.Dispatch.Record.Attempt.RequestID != capsule.RequestID || !reflect.DeepEqual(dispatch.Pod, s.pod(c.SandboxID, c.IntentID, c.Generation)) {
		return nil, ErrPlainColdProfile
	}
	r := history.Publication.Record
	if r.NodeUID != s.config.Enrollment.NodeUID || r.NodeBootID != s.config.Enrollment.NodeBootID || c.Runtime.ID != dispatch.Pod.Spec.ID {
		return nil, ErrPlainColdProfile
	}
	var business *sandboxsnapshot.Snapshot
	if capsule.Version == 2 {
		business, err = sandboxsnapshot.Decode(capsule.Business)
		if err != nil {
			return nil, ErrPlainColdProfile
		}
	}
	return &plainColdLoadedTarget{view: read.View, control: c, capsule: capsule, dispatch: dispatch, history: history, business: business, snapshot: read.Snapshot}, nil
}

// Lookup uses one original admission through metadata, retained history, the
// authenticated native exchange and final fences. It has no write side effects.
func (s *PlainColdService) Lookup(ctx context.Context, sandboxID string) (*PlainColdLookupResult, error) {
	call, err := s.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer s.leave(call)
	ctx = call.context
	released, err := s.backend.ReadSandboxReleased(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	if released != nil {
		return nil, ErrPlainColdNotFound
	}
	loaded, err := s.loadColdTarget(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	c, capsule, dispatch, history := loaded.control, loaded.capsule, loaded.dispatch, loaded.history
	identity := p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: p.RuntimeReference(*c.Runtime)}
	enrollment := history.Publication.Enrollment.Request
	destination, credential, err := s.destination(s.target(dispatch.Pod, c.Runtime.UID), state.ColdBootstrapMaterial{RuntimeCertificate: enrollment.RuntimeCertificate, IssuerCertificate: enrollment.IssuerCertificate}, identity)
	if err != nil {
		return nil, err
	}
	defer credential.Close()
	checkTTL := func() error {
		now, err := s.now(ctx)
		if err != nil {
			return err
		}
		return checkPlainColdLookupTTL(now, c.ExpiresAt)
	}
	if err := checkTTL(); err != nil {
		return nil, err
	}
	health, err := destination.QueryRuntimeHealth(ctx, c.DataGateEpoch)
	if err != nil {
		return nil, err
	}
	if health.GateState != "open" {
		return nil, ErrPlainColdUnavailable
	}
	if err := checkTTL(); err != nil {
		return nil, err
	}
	if err := s.backend.ConfirmSandboxView(ctx, loaded.view); err != nil {
		return nil, err
	}
	if err := checkTTL(); err != nil {
		return nil, err
	}
	result := &PlainColdLookupResult{RequestID: capsule.RequestID, IntentID: c.IntentID, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: identity.Runtime, CreatedAt: dispatch.CreatedAt, ExpiresAt: c.ExpiresAt, Business: loaded.business}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, context.Canceled
	}
	return result, nil
}

package provisioning

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPlainColdBusinessKeyEchoAndAdmission(t *testing.T) {
	var absent *PlainColdService
	result, err := absent.CreateBusiness(context.Background(), PlainColdBusinessInput{Request: types.CreateSandboxRequest{Mode: "ephemeral"}})
	require.Error(t, err)
	parsed, parseErr := uuid.Parse(result.IdempotencyKey)
	require.NoError(t, parseErr, "even admission failure retains the generated key for attribution")
	require.NotEqual(t, uuid.Nil, parsed)
	result, err = absent.CreateBusiness(context.Background(), PlainColdBusinessInput{IdempotencyKey: "caller-key"})
	require.Error(t, err)
	require.Equal(t, "caller-key", result.IdempotencyKey)
	s, _, _ := coldBusinessFixture(t)
	result, err = s.CreateBusiness(context.Background(), PlainColdBusinessInput{IdempotencyKey: "key", Request: types.CreateSandboxRequest{Mode: "ephemeral"}})
	require.ErrorIs(t, err, ErrPlainColdProfile, "business configuration is opt-in")
	require.Equal(t, "key", result.IdempotencyKey)
	require.Empty(t, s.active, "the single admission must join on rejection")
	s.closed = true
	_, err = s.CreateBusiness(context.Background(), PlainColdBusinessInput{IdempotencyKey: "key"})
	require.Error(t, err, "Close seals business lookup admission")
}

// These pure recovery checks catch accepting a foreign capsule or recomputing
// incoming defaults from a changed deployment before original-request replay.
func TestPlainColdBusinessReplayOriginalConfiguration(t *testing.T) {
	s, _, original := coldBusinessFixture(t)
	w, err := state.NewDetachedIdentity("storage", original.Principal, original.IdempotencyKey)
	require.NoError(t, err)
	original.Business = coldBusinessSnapshot(t, types.CreateSandboxRequest{Mode: "ephemeral"})
	wire, err := coldRequestCapsule(s.profile, w, original)
	require.NoError(t, err)
	record := state.CreationRequestRecord{RequestID: original.RequestID, IntentID: original.IntentID, SandboxID: original.SandboxID, WorkspaceHash: w.Hash()}
	ref := state.SnapshotReference{Version: original.SnapshotVersion, Digest: strings.Repeat("b", 64)}
	recovered, err := s.replayBusinessRequest(w, record, ref, wire, types.CreateSandboxRequest{Mode: "ephemeral"})
	require.NoError(t, err)
	require.Equal(t, original.RequestID, recovered.RequestID)
	require.Equal(t, original.IntentID, recovered.IntentID)
	require.Equal(t, original.SandboxID, recovered.SandboxID)
	require.Equal(t, original.SnapshotVersion, recovered.SnapshotVersion)
	require.Equal(t, time.Minute, recovered.TTL)
	require.Equal(t, original.Business, recovered.Business)
	recovered.Business.Resolved.Resources.CPU = "changed"
	require.Equal(t, "2", original.Business.Resolved.Resources.CPU, "result has independent configuration ownership")
	_, err = s.replayBusinessRequest(w, record, ref, wire, types.CreateSandboxRequest{Mode: "ephemeral", Timeout: 120})
	require.ErrorIs(t, err, state.ErrIdempotencyConflict)
	_, err = s.replayBusinessRequest(w, record, ref, wire, types.CreateSandboxRequest{Mode: "persistent"})
	require.ErrorIs(t, err, state.ErrIdempotencyConflict)
	for name, mutate := range map[string]func(*plainColdCapsule){
		"legacy":     func(c *plainColdCapsule) { c.Version, c.Business = 1, nil },
		"request":    func(c *plainColdCapsule) { c.RequestID = uuid.NewString() },
		"intent":     func(c *plainColdCapsule) { c.IntentID = uuid.NewString() },
		"sandbox":    func(c *plainColdCapsule) { c.SandboxID = uuid.NewString() },
		"version":    func(c *plainColdCapsule) { c.SnapshotVersion = uuid.NewString() },
		"deployment": func(c *plainColdCapsule) { c.Deployment = strings.Repeat("c", 64) },
		"partition":  func(c *plainColdCapsule) { c.Partition++ },
		"workspace":  func(c *plainColdCapsule) { c.WorkspaceHash = strings.Repeat("c", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			var c plainColdCapsule
			require.NoError(t, decodeColdCapsule(wire, &c))
			mutate(&c)
			bad, err := json.Marshal(c)
			require.NoError(t, err)
			_, err = s.replayBusinessRequest(w, record, ref, bad, types.CreateSandboxRequest{Mode: "ephemeral"})
			require.Error(t, err)
		})
	}
}

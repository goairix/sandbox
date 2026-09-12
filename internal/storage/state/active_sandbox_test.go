package state

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActiveSandboxRecordValidate(t *testing.T) {
	now := time.Now().UTC()
	valid := ActiveSandboxRecord{
		Version: 1, SandboxID: "sandbox-a", Phase: ActiveSandboxActive,
		Revision: 2, Generation: 1, RuntimeID: "pod-a", RuntimeUID: "uid-a",
		Snapshot: []byte(`{"id":"sandbox-a"}`), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name string
		edit func(*ActiveSandboxRecord)
	}{
		{"zero version", func(r *ActiveSandboxRecord) { r.Version = 0 }},
		{"empty sandbox", func(r *ActiveSandboxRecord) { r.SandboxID = "" }},
		{"invalid phase", func(r *ActiveSandboxRecord) { r.Phase = "unknown" }},
		{"zero revision", func(r *ActiveSandboxRecord) { r.Revision = 0 }},
		{"zero generation", func(r *ActiveSandboxRecord) { r.Generation = 0 }},
		{"empty runtime", func(r *ActiveSandboxRecord) { r.RuntimeID = "" }},
		{"empty runtime uid", func(r *ActiveSandboxRecord) { r.RuntimeUID = "" }},
		{"empty snapshot", func(r *ActiveSandboxRecord) { r.Snapshot = nil }},
		{"missing created time", func(r *ActiveSandboxRecord) { r.CreatedAt = time.Time{} }},
		{"missing updated time", func(r *ActiveSandboxRecord) { r.UpdatedAt = time.Time{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := valid
			tt.edit(&record)
			assert.ErrorIs(t, record.Validate(), ErrActiveSandboxCorrupt)
		})
	}
}

func TestActiveSandboxLeaseValidation(t *testing.T) {
	now := time.Now().UTC()
	op := ActiveSandboxOperation{SandboxID: "sandbox-a", Token: "token-a", Generation: 1, Kind: ActiveOperationData, ExpiresAt: now.Add(time.Second)}
	require.NoError(t, op.Validate(now))

	badKind := op
	badKind.Kind = "invalid"
	assert.ErrorIs(t, badKind.Validate(now), ErrActiveSandboxCorrupt)

	expired := op
	expired.ExpiresAt = now
	assert.ErrorIs(t, expired.Validate(now), ErrActiveSandboxLeaseExpired)

	controller := ActiveSandboxControllerLease{SandboxID: "sandbox-a", Token: "token-a", InstanceID: "api-a", PodUID: "pod-uid-a", Generation: 1, ExpiresAt: now.Add(time.Second)}
	require.NoError(t, controller.Validate(now))
	controller.Generation = 0
	assert.ErrorIs(t, controller.Validate(now), ErrActiveSandboxCorrupt)
}

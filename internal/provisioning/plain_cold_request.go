package provisioning

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
)

var ErrPlainColdProfile = errors.New("unsupported plain cold creation profile")

// PlainColdRequest requires already resolved, positive TTL and stable request
// identifiers. The application adapter retains principal/workspace authorization.
type PlainColdRequest struct {
	Principal, IdempotencyKey, RequestID, IntentID, SandboxID, SnapshotVersion string
	TTL                                                                        time.Duration
}

type plainColdCapsule struct {
	Version         int           `json:"version"`
	Deployment      string        `json:"deployment"`
	WorkspaceHash   string        `json:"workspace_hash"`
	Partition       uint8         `json:"partition"`
	RequestID       string        `json:"request_id"`
	IntentID        string        `json:"intent_id"`
	SandboxID       string        `json:"sandbox_id"`
	SnapshotVersion string        `json:"snapshot_version"`
	TTL             time.Duration `json:"ttl"`
}

func coldRequestCapsule(deployment string, w state.WorkspaceIdentity, r PlainColdRequest) (json.RawMessage, error) {
	if !coldDigest(deployment) || !coldDigest(w.Hash()) || r.TTL <= 0 || r.TTL > 365*24*time.Hour || r.Principal == "" || r.IdempotencyKey == "" {
		return nil, ErrPlainColdProfile
	}
	for _, id := range []string{r.RequestID, r.IntentID, r.SandboxID, r.SnapshotVersion} {
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
			return nil, ErrPlainColdProfile
		}
	}
	return json.Marshal(plainColdCapsule{Version: 1, Deployment: deployment, WorkspaceHash: w.Hash(), Partition: w.Partition(), RequestID: r.RequestID, IntentID: r.IntentID, SandboxID: r.SandboxID, SnapshotVersion: r.SnapshotVersion, TTL: r.TTL})
}

func coldDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}

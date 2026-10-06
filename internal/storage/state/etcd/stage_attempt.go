package etcd

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// StageAttemptLocator identifies an internally chosen metadata attempt before
// its mutation exists. It omits the digest so mutations can safely persist it.
type StageAttemptLocator struct {
	Namespace    string `json:"namespace"`
	Partition    uint8  `json:"partition"`
	RequestID    string `json:"request_id"`
	StageID      string `json:"stage_id"`
	AttemptID    string `json:"attempt_id"`
	RestoreEpoch string `json:"restore_epoch"`
}

// Validate rejects noncanonical namespace roots and malformed attempt identity.
func (l StageAttemptLocator) Validate() error {
	invalid := func() error { return fmt.Errorf("%w: invalid stage attempt locator", ErrInvalidMutation) }
	if len(l.Namespace) > maxNamespaceRootBytes || !strings.HasSuffix(l.Namespace, "/") {
		return invalid()
	}
	parts := strings.Split(strings.TrimSuffix(l.Namespace, "/"), "/")
	if len(parts) < 4 {
		return invalid()
	}
	n, err := NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	if err != nil || n.Root() != l.Namespace {
		return invalid()
	}
	for _, id := range []string{l.RequestID, l.StageID, l.RestoreEpoch} {
		if !validSegment(id) || len(id) > 128 {
			return invalid()
		}
	}
	if id, err := uuid.Parse(l.AttemptID); err != nil || id.String() != l.AttemptID {
		return invalid()
	}
	return nil
}

func (l StageAttemptLocator) reference(digest string) StageReference {
	return StageReference{Namespace: l.Namespace, Partition: l.Partition, RequestID: l.RequestID, StageID: l.StageID, AttemptID: l.AttemptID, Digest: digest, RestoreEpoch: l.RestoreEpoch}
}

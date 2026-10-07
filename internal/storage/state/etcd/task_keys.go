package etcd

import "fmt"

func (n Namespace) taskKeyParts(r TaskReference) ([]string, error) {
	if r.Validate() != nil {
		return nil, ErrInvalidRecord
	}
	if r.Namespace != n.Root() {
		return nil, ErrIdentityMismatch
	}
	return []string{"p", fmt.Sprintf("%02x", r.Partition), "tasks", r.TaskID}, nil
}
func (n Namespace) taskKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(parts...)
}
func (n Namespace) cleanupIntentKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	parts[2] = "cleanup-intents"
	return n.Key(parts...)
}
func (n Namespace) taskLinkKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	parts[2] = "sandboxes"
	parts[3] = r.SandboxID
	return n.Key(append(parts, "cleanup-task")...)
}
func (n Namespace) taskClaimKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(append(parts, "claim")...)
}
func (n Namespace) taskGuardKey(r TaskReference, claimID string) (string, error) {
	if !validPreparationUUID(claimID) {
		return "", ErrInvalidRecord
	}
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(append(parts, "guards", claimID)...)
}
func (n Namespace) taskCheckpointKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(append(parts, "checkpoint")...)
}

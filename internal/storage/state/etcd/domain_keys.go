package etcd

import "fmt"

func (n Namespace) workspaceKeys(w WorkspaceIdentity) (owner, fence string, err error) {
	if err = w.validate(); err != nil {
		return "", "", err
	}
	p := fmt.Sprintf("%02x", w.Partition())
	owner, err = n.Key("p", p, "workspaces", w.Hash(), "owner")
	if err != nil {
		return "", "", err
	}
	fence, err = n.Key("p", p, "workspaces", w.Hash(), "fence")
	if err != nil {
		return "", "", err
	}
	return owner, fence, nil
}
func (n Namespace) sandboxKeys(partition uint8, sandboxID, snapshotVersion string) (control, snapshot string, err error) {
	if !validDomainSegment(sandboxID) || !validDomainSegment(snapshotVersion) {
		return "", "", fmt.Errorf("%w: invalid sandbox or snapshot identity", ErrInvalidRecord)
	}
	p := fmt.Sprintf("%02x", partition)
	control, err = n.Key("p", p, "controls", sandboxID)
	if err != nil {
		return "", "", err
	}
	snapshot, err = n.Key("p", p, "snapshots", sandboxID, snapshotVersion)
	if err != nil {
		return "", "", err
	}
	return control, snapshot, nil
}
func (n Namespace) intentKey(partition uint8, intentID string) (string, error) {
	if !validDomainSegment(intentID) {
		return "", fmt.Errorf("%w: invalid intent identity", ErrInvalidRecord)
	}
	return n.Key("p", fmt.Sprintf("%02x", partition), "intents", intentID)
}
func (n Namespace) requestKey(hash string) (string, error) {
	if !validHexDigest(hash) {
		return "", fmt.Errorf("%w: invalid request hash", ErrInvalidRecord)
	}
	return n.Key("requests", hash)
}
func (n Namespace) placementKey(sandboxID string) (string, error) {
	if !validDomainSegment(sandboxID) {
		return "", fmt.Errorf("%w: invalid sandbox placement identity", ErrInvalidRecord)
	}
	return n.Key("indexes", "sandbox", sandboxID)
}

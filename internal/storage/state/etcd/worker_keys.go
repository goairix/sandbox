package etcd

import "fmt"

func (n Namespace) workerSessionKey(sessionID string) (string, error) {
	if !validPreparationUUID(sessionID) {
		return "", ErrInvalidRecord
	}
	return n.Key("workers", "sessions", sessionID)
}

func (n Namespace) workerPartitionKey(partition uint8) (string, error) {
	return n.Key("p", fmt.Sprintf("%02x", partition), "worker")
}

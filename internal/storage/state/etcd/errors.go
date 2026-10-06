package etcd

import "errors"

var (
	ErrInvalidConfiguration = errors.New("etcd state: invalid configuration")
	ErrIdentityMismatch     = errors.New("etcd state: identity mismatch")
	ErrConflict             = errors.New("etcd state: conflict")
	ErrGuardExpired         = errors.New("etcd state: guard expired")
	ErrOutcomeUnknown       = errors.New("etcd state: outcome unknown")
	ErrInvalidMutation      = errors.New("etcd state: invalid mutation")
	ErrCorruptReceipt       = errors.New("etcd state: corrupt receipt")
)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ProvisionWorkspaceControlKey installs the operator's original 32-byte seed.
// It never generates, replaces, deletes, or rotates a key. An ambiguous write
// has one read-only arbitration attempt, never another application CAS.
func (b *Backend) ProvisionWorkspaceControlKey(ctx context.Context, seed []byte) (ed25519.PublicKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, ErrInvalidRecord
	}
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	owned := bytes.Clone(seed)
	defer clear(owned)
	name, err := b.namespace.Key("meta", "workspace-control-seed-v1")
	if err != nil {
		return nil, err
	}
	if err := b.controlKeyPreflight(name, true); err != nil {
		return nil, err
	}
	put := clientv3.OpPut(name, string(owned))
	defer clear(put.ValueBytes())
	rpcCtx, cancel := b.requestContext(ctx)
	r, rpcErr := b.client.Txn(rpcCtx).If(append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(name), "=", 0))...).Then(put).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(name)).Commit()
	deadlineErr := rpcCtx.Err()
	cancel()
	if rpcErr != nil || deadlineErr != nil {
		// A timeout or lost ACK is not authority to replay the write. The new
		// bounded point read must independently pass the current operator fence.
		observed, readErr := b.controlKeyRead(ctx, name)
		if readErr != nil {
			return nil, errors.Join(ErrOutcomeUnknown, readErr)
		}
		defer clear(observed)
		if !bytes.Equal(observed, owned) {
			return nil, ErrConflict
		}
	} else {
		if err := b.controlKeyHeader(r); err != nil {
			return nil, err
		}
		if r.Succeeded {
			if err := b.controlKeyPutResponse(r); err != nil {
				return nil, err
			}
		} else {
			points, err := b.readViewResponse(r, []string{b.identityKey, b.restoreKey, name}, []int{maxIdentityJSONBytes, maxIdentityFieldBytes, ed25519.SeedSize})
			defer clearControlKeyPoints(points)
			if err != nil {
				return nil, err
			}
			if points[0] == nil || points[1] == nil || string(points[0].Value) != b.identityValue || string(points[1].Value) != b.restoreEpoch {
				return nil, ErrIdentityMismatch
			}
			if err := controlKeyRecord(points[2]); err != nil {
				return nil, err
			}
			if !bytes.Equal(points[2].Value, owned) {
				return nil, ErrConflict
			}
		}
	}
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	key := ed25519.NewKeyFromSeed(owned)
	defer clear(key)
	return bytes.Clone(key[ed25519.SeedSize:]), nil
}

// LoadWorkspaceControlKey performs one bounded, linearizable, operator-fenced
// point read for startup. The caller owns the returned key and its lifetime.
func (b *Backend) LoadWorkspaceControlKey(ctx context.Context) (ed25519.PrivateKey, error) {
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	name, err := b.namespace.Key("meta", "workspace-control-seed-v1")
	if err != nil {
		return nil, err
	}
	seed, err := b.controlKeyRead(ctx, name)
	if err != nil {
		return nil, err
	}
	defer clear(seed)
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func (b *Backend) controlKeyInput(ctx context.Context) error {
	if err := b.readViewInput(ctx); err != nil {
		return err
	}
	if err := b.client.Ctx().Err(); err != nil {
		return err
	}
	return nil
}

func controlKeyRecord(kv *mvccpb.KeyValue) error {
	if kv == nil {
		return ErrInvalidRecord
	}
	if len(kv.Value) != ed25519.SeedSize || kv.Lease != 0 || kv.Version != 1 || kv.CreateRevision <= 0 || kv.CreateRevision != kv.ModRevision {
		return ErrCorruptRecord
	}
	return nil
}

func clearControlKeyPoints(points []*mvccpb.KeyValue) {
	for _, point := range points {
		if point != nil {
			clear(point.Value)
		}
	}
}

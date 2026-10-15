package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func controlKeySeed() []byte { return bytes.Repeat([]byte{0x71}, ed25519.SeedSize) }

func TestControlKeyProvisionAndOwnedReload(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	expected := ed25519.NewKeyFromSeed(seed)
	defer clear(expected)
	pub, err := b.ProvisionWorkspaceControlKey(ctx, seed)
	if err != nil {
		t.Fatalf("provision refused: %v", err)
	}
	if !bytes.Equal(pub, expected.Public().(ed25519.PublicKey)) {
		t.Fatal("provision public key mismatch")
	}
	clear(seed)
	clear(pub)
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		t.Fatal(err)
	}
	identity.RestoreEpoch = b.restoreEpoch
	reopened, err := New(ctx, Options{Endpoints: raw.Endpoints(), Namespace: b.namespace, Identity: identity, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keyName := b.namespace.Root() + "meta/workspace-control-seed-v1"
	before, err := raw.Get(ctx, keyName)
	if err != nil || len(before.Kvs) != 1 {
		t.Fatal("missing durable seed")
	}
	key, err := reopened.LoadWorkspaceControlKey(ctx)
	if err != nil || !bytes.Equal(key, expected) {
		t.Fatal("cross-backend load mismatch")
	}
	clear(key)
	key, err = b.LoadWorkspaceControlKey(ctx)
	if err != nil || !bytes.Equal(key, expected) {
		t.Fatal("returned key aliases durable seed")
	}
	clear(key)
	after, err := raw.Get(ctx, keyName)
	if err != nil || len(after.Kvs) != 1 || before.Kvs[0].ModRevision != after.Kvs[0].ModRevision || after.Kvs[0].Version != 1 || after.Kvs[0].Lease != 0 || after.Kvs[0].CreateRevision != after.Kvs[0].ModRevision {
		t.Fatal("load changed immutable record")
	}
}

func TestControlKeyConcurrentIdempotenceAndConflict(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	seed := controlKeySeed()
	defer clear(seed)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.ProvisionWorkspaceControlKey(ctx, seed)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same seed refused: %v", err)
		}
	}
	name := b.namespace.Root() + "meta/workspace-control-seed-v1"
	before, err := raw.Get(ctx, name)
	if err != nil || len(before.Kvs) != 1 {
		t.Fatal("missing concurrent initialization")
	}
	other := bytes.Repeat([]byte{0x72}, ed25519.SeedSize)
	defer clear(other)
	if pub, err := b.ProvisionWorkspaceControlKey(ctx, other); err == nil || pub != nil {
		t.Fatal("different seed accepted")
	}
	after, err := raw.Get(ctx, name)
	if err != nil || len(after.Kvs) != 1 || before.Kvs[0].ModRevision != after.Kvs[0].ModRevision || after.Kvs[0].Version != 1 || !bytes.Equal(after.Kvs[0].Value, seed) {
		t.Fatal("concurrent or conflicting provision changed record")
	}
}

func TestControlKeyLoadMissingIsReadOnly(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	key, err := b.LoadWorkspaceControlKey(ctx)
	if err == nil || key != nil {
		t.Fatal("missing seed returned a key")
	}
	got, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
	if err != nil || len(got.Kvs) != 2 {
		t.Fatal("missing load wrote state")
	}
}

package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// integrationBackend provisions a unique, disposable operator namespace on a
// real three-member cluster. Production New never performs this provisioning.
func integrationBackend(t *testing.T) (*Backend, *clientv3.Client) {
	t.Helper()
	endpointsEnv := os.Getenv("TEST_ETCD_ENDPOINTS")
	if endpointsEnv == "" {
		t.Skip("set TEST_ETCD_ENDPOINTS to a real three-member etcd cluster")
	}
	endpoints := strings.Split(endpointsEnv, ",")
	if len(endpoints) != 3 {
		t.Fatal("TEST_ETCD_ENDPOINTS must contain exactly three endpoints")
	}
	for i := range endpoints {
		endpoints[i] = strings.TrimSpace(endpoints[i])
	}
	raw, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	members, err := raw.MemberList(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if len(members.Members) != 3 {
		t.Fatalf("fixture has %d members, want 3", len(members.Members))
	}
	memberIDs := make(map[uint64]bool, 3)
	for _, member := range members.Members {
		memberIDs[member.ID] = true
	}
	endpointMembers := make(map[uint64]bool, 3)
	for _, endpoint := range endpoints {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		status, err := raw.Status(ctx, endpoint)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if status.Header == nil || status.Header.ClusterId != members.Header.ClusterId {
			t.Fatalf("endpoint %s is not in the fixture cluster", endpoint)
		}
		if !memberIDs[status.Header.MemberId] {
			t.Fatalf("endpoint %s is not a listed cluster member", endpoint)
		}
		endpointMembers[status.Header.MemberId] = true
		t.Logf("etcd fixture endpoint=%s member=%x cluster=%x server=%s", endpoint, status.Header.MemberId, status.Header.ClusterId, status.Version)
	}
	if len(endpointMembers) != 3 {
		t.Fatalf("fixture endpoints reach %d distinct members, want 3", len(endpointMembers))
	}
	n, err := NewNamespace("/codex-test", "state", fmt.Sprintf("case-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := raw.Delete(ctx, n.Root(), clientv3.WithPrefix()); err != nil {
			t.Errorf("cleanup test namespace: %v", err)
		}
	})
	identity := Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: members.Header.ClusterId, StorageID: "test-storage", RuntimeID: "test-runtime", RestoreEpoch: "test-epoch"}
	value, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	identityKey, _ := n.Key("meta", "identity")
	restoreKey, _ := n.Key("meta", "restore_epoch")
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(identityKey, string(value)), clientv3.OpPut(restoreKey, identity.RestoreEpoch)).Commit()
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New(context.Background(), Options{Endpoints: endpoints, Namespace: n, Identity: identity, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend, raw
}

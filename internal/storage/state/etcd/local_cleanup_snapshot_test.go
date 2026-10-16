package etcd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This independent wire oracle deliberately does not call the production
// snapshot or acquisition digest helpers. It fixes their domain/framing contract.
func localCleanupTestDigest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func localCleanupTestJSON(t *testing.T, value any) string {
	t.Helper()
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	require.NoError(t, encoder.Encode(value))
	return strings.TrimSuffix(out.String(), "\n")
}

func localCleanupTestInput(t *testing.T, suffix string, exclude []string, deleteExtraneous bool) AcquireIntentInput {
	t.Helper()
	input := acquisitionInput(t, suffix)
	w, err := NewWorkspaceIdentity("local", "test-storage", "bucket", "team/project/")
	require.NoError(t, err)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, Exclude: exclude, DeleteExtraneous: deleteExtraneous})
	require.NoError(t, err)
	input.Workspace, input.LocalCleanupPlan = w, &plan
	return input
}

func localCleanupTestRecord(t *testing.T) SandboxSnapshotRecord {
	t.Helper()
	w, err := NewWorkspaceIdentity("local", "test-storage", "bucket", "team/project/")
	require.NoError(t, err)
	d := &LocalCleanupDescriptor{
		Version: 1, Domain: "sandbox.local-cleanup-descriptor.v1", Provider: "local",
		StorageIdentityHash: w.storageIdentityHash, Bucket: "bucket", Prefix: "team/project/",
		WorkspaceHash: w.Hash(), Partition: w.Partition(), Exclude: []string{"cache", "tmp"}, DeleteExtraneous: true,
	}
	payload := json.RawMessage(`{"text":"<>&","id":9007199254740993}`)
	digest := localCleanupTestDigest("sandbox.snapshot.v2", "2", string(payload), localCleanupTestJSON(t, d))
	return SandboxSnapshotRecord{Version: 2, SandboxID: "sandbox", Snapshot: SnapshotReference{Version: "snapshot", Digest: digest}, Payload: payload, LocalCleanup: d}
}

func TestLocalCleanupSnapshotCodec(t *testing.T) {
	t.Run("legacy golden remains unchanged", func(t *testing.T) {
		record := SandboxSnapshotRecord{Version: 1, SandboxID: "sandbox", Snapshot: SnapshotReference{Version: "snapshot", Digest: "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"}, Payload: json.RawMessage(`{}`)}
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		require.Equal(t, `{"version":1,"sandbox_id":"sandbox","snapshot":{"version":"snapshot","digest":"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"},"payload":{}}`, string(wire))
		var decoded SandboxSnapshotRecord
		require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &decoded))
		require.Equal(t, record, decoded)

		wire = wire[:len(wire)-1] + `,"local_cleanup":null}`
		require.ErrorIs(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &decoded), ErrCorruptRecord)
		require.Equal(t, record, decoded, "failed decode must not publish a partial result")
	})

	t.Run("v2 canonical envelope and pointer encoding", func(t *testing.T) {
		record := localCleanupTestRecord(t)
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		pointerWire, err := encodeDomainRecord(&record)
		require.NoError(t, err)
		require.Equal(t, wire, pointerWire)
		require.Equal(t, localCleanupTestJSON(t, record), string(wire))
		require.Contains(t, string(wire), `"text":"<>&"`)
		require.NotContains(t, string(wire), "test-storage", "only the storage identity hash may be persisted")
		var decoded SandboxSnapshotRecord
		backing := []byte(wire)
		require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: backing}, &decoded))
		require.Equal(t, record, decoded)
		for i := range backing {
			backing[i] = 'x'
		}
		require.Equal(t, record, decoded, "decoded payload and descriptor must own their backing")
	})

	t.Run("empty policy and false deletion remain explicit", func(t *testing.T) {
		record := localCleanupTestRecord(t)
		record.LocalCleanup.Exclude = []string{}
		record.LocalCleanup.DeleteExtraneous = false
		record.Snapshot.Digest = localCleanupTestDigest("sandbox.snapshot.v2", "2", string(record.Payload), localCleanupTestJSON(t, record.LocalCleanup))
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		require.Contains(t, wire, `"exclude":[],"delete_extraneous":false`)
		var decoded SandboxSnapshotRecord
		require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &decoded))
		require.Equal(t, record, decoded)
	})

	t.Run("every descriptor field and payload is digest bound", func(t *testing.T) {
		changes := map[string]func(*SandboxSnapshotRecord){
			"version":   func(r *SandboxSnapshotRecord) { r.LocalCleanup.Version++ },
			"domain":    func(r *SandboxSnapshotRecord) { r.LocalCleanup.Domain += ".other" },
			"provider":  func(r *SandboxSnapshotRecord) { r.LocalCleanup.Provider = "s3" },
			"storage":   func(r *SandboxSnapshotRecord) { r.LocalCleanup.StorageIdentityHash = strings.Repeat("a", 64) },
			"bucket":    func(r *SandboxSnapshotRecord) { r.LocalCleanup.Bucket = "other" },
			"prefix":    func(r *SandboxSnapshotRecord) { r.LocalCleanup.Prefix = "other/" },
			"workspace": func(r *SandboxSnapshotRecord) { r.LocalCleanup.WorkspaceHash = strings.Repeat("b", 64) },
			"partition": func(r *SandboxSnapshotRecord) { r.LocalCleanup.Partition++ },
			"exclude":   func(r *SandboxSnapshotRecord) { r.LocalCleanup.Exclude[0] = "changed" },
			"delete":    func(r *SandboxSnapshotRecord) { r.LocalCleanup.DeleteExtraneous = false },
			"payload":   func(r *SandboxSnapshotRecord) { r.Payload = json.RawMessage(`{"other":true}`) },
		}
		for name, change := range changes {
			t.Run(name, func(t *testing.T) {
				record := localCleanupTestRecord(t)
				change(&record)
				_, err := encodeDomainRecord(record)
				require.Error(t, err)
			})
		}
	})

	t.Run("strict shape and bounded envelope", func(t *testing.T) {
		record := localCleanupTestRecord(t)
		canonical := localCleanupTestJSON(t, record)
		cases := map[string]string{
			"duplicate root":       strings.Replace(canonical, `"version":2`, `"version":2,"version":2`, 1),
			"unknown root":         strings.Replace(canonical, `"version":2`, `"version":2,"extra":true`, 1),
			"duplicate descriptor": strings.Replace(canonical, `"provider":"local"`, `"provider":"local","provider":"local"`, 1),
			"unknown descriptor":   strings.Replace(canonical, `"provider":"local"`, `"provider":"local","extra":true`, 1),
			"null descriptor":      strings.Replace(canonical, localCleanupTestJSON(t, record.LocalCleanup), "null", 1),
			"missing descriptor":   strings.Split(canonical, `,"local_cleanup":`)[0] + "}",
			"null excludes":        strings.Replace(canonical, `["cache","tmp"]`, "null", 1),
			"missing false":        strings.Replace(canonical, `,"delete_extraneous":true`, "", 1),
			"partition width":      strings.Replace(canonical, fmt.Sprintf(`"partition":%d`, record.LocalCleanup.Partition), `"partition":256`, 1),
			"unsorted excludes":    strings.Replace(canonical, `["cache","tmp"]`, `["tmp","cache"]`, 1),
			"duplicate excludes":   strings.Replace(canonical, `["cache","tmp"]`, `["cache","cache"]`, 1),
			"covered excludes":     strings.Replace(canonical, `["cache","tmp"]`, `["cache","cache/child"]`, 1),
			"unsupported version":  strings.Replace(canonical, `"version":2`, `"version":3`, 1),
			"trailing value":       canonical + `{}`,
			"invalid utf8":         canonical + string([]byte{0xff}),
			"oversize":             canonical + strings.Repeat(" ", 65536),
		}
		for name, wire := range cases {
			t.Run(name, func(t *testing.T) {
				before := SandboxSnapshotRecord{SandboxID: "unchanged"}
				got := before
				require.ErrorIs(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &got), ErrCorruptRecord)
				require.Equal(t, before, got)
			})
		}
		var got SandboxSnapshotRecord
		require.ErrorIs(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(canonical), Lease: 1}, &got), ErrCorruptRecord)

		// These paths satisfy the existing plan limits, but their descriptor
		// exceeds the independently frozen 8192-byte persisted envelope.
		record.LocalCleanup.Exclude = []string{strings.Repeat("a", 4000), strings.Repeat("b", 4000)}
		descriptor := localCleanupTestJSON(t, record.LocalCleanup)
		require.Greater(t, len(descriptor), 8192)
		record.Snapshot.Digest = localCleanupTestDigest("sandbox.snapshot.v2", "2", string(record.Payload), descriptor)
		_, err := encodeDomainRecord(record)
		require.Error(t, err)
	})
}

func TestLocalCleanupSnapshotAcquire(t *testing.T) {
	t.Run("atomic v2 graph and independent digests", func(t *testing.T) {
		b, raw := integrationBackend(t)
		ctx := context.Background()
		input := localCleanupTestInput(t, "v2", []string{"tmp", "cache/child", "cache", "tmp"}, false)
		result, err := b.AcquireIntent(ctx, input)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, result.Outcome)
		control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
		require.NoError(t, err)
		snapshot, err := b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
		require.NoError(t, err)
		require.Equal(t, uint32(2), snapshot.Version)
		require.NotNil(t, snapshot.LocalCleanup)
		require.Equal(t, []string{"cache", "tmp"}, snapshot.LocalCleanup.Exclude)
		require.False(t, snapshot.LocalCleanup.DeleteExtraneous)
		require.Equal(t, input.Workspace.Hash(), snapshot.LocalCleanup.WorkspaceHash)
		require.Equal(t, input.Workspace.Partition(), snapshot.LocalCleanup.Partition)
		digest := localCleanupTestDigest("sandbox.snapshot.v2", "2", string(input.Payload), localCleanupTestJSON(t, snapshot.LocalCleanup))
		require.Equal(t, digest, control.Snapshot.Digest)
		configuration := localCleanupTestDigest("sandbox.acquire-configuration.v2", "2", input.Workspace.Hash(), digest, strconv.FormatInt(int64(input.TTL), 10))
		require.Equal(t, configuration, result.Request.ConfigurationDigest)
		intent, err := b.LoadCreationIntent(ctx, input.Workspace, input.IntentID)
		require.NoError(t, err)
		require.Equal(t, configuration, intent.ConfigurationDigest)

		ownerKey, fenceKey, err := b.namespace.workspaceKeys(input.Workspace)
		require.NoError(t, err)
		controlKey, snapshotKey, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
		require.NoError(t, err)
		intentKey, err := b.namespace.intentKey(input.Workspace.Partition(), input.IntentID)
		require.NoError(t, err)
		hash, err := requestKeyHash(input.Principal, input.IdempotencyKey)
		require.NoError(t, err)
		requestKey, err := b.namespace.requestKey(hash)
		require.NoError(t, err)
		placementKey, err := b.namespace.placementKey(input.SandboxID)
		require.NoError(t, err)
		_, receiptKey, err := b.stageKeys(result.Reference)
		require.NoError(t, err)
		var birth int64
		for _, key := range []string{ownerKey, fenceKey, controlKey, snapshotKey, intentKey, requestKey, placementKey, receiptKey} {
			response, err := raw.Get(ctx, key)
			require.NoError(t, err)
			require.Len(t, response.Kvs, 1)
			kv := response.Kvs[0]
			if birth == 0 {
				birth = kv.ModRevision
			}
			require.Equal(t, birth, kv.CreateRevision)
			require.Equal(t, birth, kv.ModRevision)
			require.Zero(t, kv.Lease)
		}
	})

	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay legacy=%t", legacy), func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			input := localCleanupTestInput(t, "replay", nil, true)
			if legacy {
				input.LocalCleanupPlan = nil
			}
			first, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			before, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			lease := &acquisitionLease{Lease: b.client.Lease}
			b.client.Lease = lease
			retry := localCleanupTestInput(t, "replay", nil, true)
			// The retry proposes fresh IDs; the accepted request remains original.
			replayed, err := b.AcquireIntent(ctx, retry)
			require.NoError(t, err)
			require.Equal(t, AcquireReplay, replayed.Disposition)
			require.Equal(t, first.Request, replayed.Request)
			require.Empty(t, replayed.Reference)
			if legacy {
				payloadDigest := sha256.Sum256(input.Payload)
				want := localCleanupTestDigest(input.Workspace.Hash(), hex.EncodeToString(payloadDigest[:]), strconv.FormatInt(int64(input.TTL), 10))
				require.Equal(t, want, first.Request.ConfigurationDigest)
			}

			changes := map[string]func(*AcquireIntentInput){
				"exclude": func(in *AcquireIntentInput) {
					plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: in.Workspace, Exclude: []string{"cache"}, DeleteExtraneous: true})
					require.NoError(t, err)
					in.LocalCleanupPlan = &plan
				},
				"delete policy": func(in *AcquireIntentInput) {
					plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: in.Workspace})
					require.NoError(t, err)
					in.LocalCleanupPlan = &plan
				},
				"payload": func(in *AcquireIntentInput) { in.Payload = json.RawMessage(`{"changed":true}`) },
				"ttl":     func(in *AcquireIntentInput) { in.TTL += time.Second },
				"workspace": func(in *AcquireIntentInput) {
					w, err := NewWorkspaceIdentity("local", "test-storage", "bucket", "different/")
					require.NoError(t, err)
					plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, DeleteExtraneous: true})
					require.NoError(t, err)
					in.Workspace, in.LocalCleanupPlan = w, &plan
				},
			}
			if !legacy {
				changes["nil downgrade"] = func(in *AcquireIntentInput) { in.LocalCleanupPlan = nil }
			}
			for name, change := range changes {
				t.Run(name, func(t *testing.T) {
					changed := retry
					change(&changed)
					_, err := b.AcquireIntent(ctx, changed)
					require.ErrorIs(t, err, ErrIdempotencyConflict)
				})
			}
			after, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Equal(t, before.Kvs, after.Kvs, "replay and policy conflict cannot rewrite or upgrade any original record")
			require.Zero(t, lease.grants)
		})
	}
}

func TestLocalCleanupSnapshotAcquireOwnsInput(t *testing.T) {
	b, raw := integrationBackend(t)
	input := localCleanupTestInput(t, "owned", []string{"cache"}, true)
	originalPayload := bytes.Clone(input.Payload)
	var first sync.Once
	b.client.KV = &faultKV{KV: b.client.KV, match: func([]clientv3.Op) bool { return true }, before: func() {
		// This synchronous first-RPC seam avoids a race in the test itself.
		// A correct acquisition has already copied both caller backing arrays.
		first.Do(func() {
			input.LocalCleanupPlan.exclude[0] = "changed"
			input.LocalCleanupPlan.deleteExtraneous = false
			input.Payload[2] = 'X'
		})
	}}
	result, err := b.AcquireIntent(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	_, key, err := b.namespace.sandboxKeys(input.Workspace.Partition(), input.SandboxID, input.SnapshotVersion)
	require.NoError(t, err)
	response, err := raw.Get(context.Background(), key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	var record SandboxSnapshotRecord
	require.NoError(t, decodeDomainRecord(response.Kvs[0], &record))
	require.Equal(t, originalPayload, []byte(record.Payload))
	require.Equal(t, []string{"cache"}, record.LocalCleanup.Exclude)
	require.True(t, record.LocalCleanup.DeleteExtraneous)
}

func TestLocalCleanupSnapshotRejectBeforeGrant(t *testing.T) {
	for _, name := range []string{"descriptor size", "envelope size", "workspace mismatch", "remote provider", "storage authority"} {
		t.Run(name, func(t *testing.T) {
			b, raw := integrationBackend(t)
			input := localCleanupTestInput(t, name, nil, true)
			switch name {
			case "descriptor size":
				plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: input.Workspace, Exclude: []string{strings.Repeat("a", 4000), strings.Repeat("b", 4000)}, DeleteExtraneous: true})
				require.NoError(t, err)
				input.LocalCleanupPlan = &plan
			case "envelope size":
				input.Payload = json.RawMessage(`{"large":"` + strings.Repeat("a", 65000) + `"}`)
			case "workspace mismatch":
				w, err := NewWorkspaceIdentity("local", "test-storage", "bucket", "other/")
				require.NoError(t, err)
				input.Workspace = w
			case "remote provider", "storage authority":
				provider, storage := "s3", "test-storage"
				if name == "storage authority" {
					provider, storage = "local", "other-storage"
				}
				w, err := NewWorkspaceIdentity(provider, storage, "bucket", "team/project/")
				require.NoError(t, err)
				plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w, DeleteExtraneous: true})
				require.NoError(t, err)
				input.Workspace, input.LocalCleanupPlan = w, &plan
			}
			lease := &acquisitionLease{Lease: b.client.Lease}
			b.client.Lease = lease
			result, err := b.AcquireIntent(context.Background(), input)
			require.Error(t, err)
			require.Empty(t, result.Reference)
			require.Zero(t, lease.grants)
			response, err := raw.Get(context.Background(), b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			require.Len(t, response.Kvs, 2, "only original operator identity and restore keys may exist")
		})
	}
}

func TestLocalCleanupSnapshotPlacementBinding(t *testing.T) {
	record := localCleanupTestRecord(t)
	wire, err := encodeDomainRecord(record)
	require.NoError(t, err)
	p := SandboxPlacementRecord{Version: 1, SandboxID: record.SandboxID, IntentID: "intent", WorkspaceHash: record.LocalCleanup.WorkspaceHash, Partition: record.LocalCleanup.Partition, RestoreEpoch: "epoch"}
	_, err = decodeSnapshot(&mvccpb.KeyValue{Value: []byte(wire)}, p.Partition, record.SandboxID, record.Snapshot, &p)
	require.NoError(t, err)
	for _, name := range []string{"workspace", "partition", "sandbox"} {
		t.Run(name, func(t *testing.T) {
			other := p
			switch name {
			case "workspace":
				other.WorkspaceHash = strings.Repeat("a", 64)
			case "partition":
				other.Partition++
			case "sandbox":
				other.SandboxID = "other"
			}
			_, err := decodeSnapshot(&mvccpb.KeyValue{Value: []byte(wire)}, p.Partition, record.SandboxID, record.Snapshot, &other)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
}

func TestLocalCleanupSnapshotCache(t *testing.T) {
	b := snapshotLifecycleBackend(t)
	record := localCleanupTestRecord(t)
	key := snapshotCacheKey{partition: record.LocalCleanup.Partition, sandboxID: record.SandboxID, version: record.Snapshot.Version, digest: record.Snapshot.Digest}
	d := record.LocalCleanup
	weight := uint64(512 + 2*(len(key.sandboxID)+len(key.version)+len(key.digest)) + len(record.SandboxID) + len(record.Snapshot.Version) + len(record.Snapshot.Digest) + len(record.Payload))
	weight += uint64(256 + len(d.Domain) + len(d.Provider) + len(d.StorageIdentityHash) + len(d.Bucket) + len(d.Prefix) + len(d.WorkspaceHash) + 32*len(d.Exclude))
	for _, path := range d.Exclude {
		weight += uint64(len(path))
	}
	c := newSnapshotReadCache(b, weight-1)
	c.store(key, &record)
	require.Zero(t, c.used)
	require.Nil(t, c.lookup(key), "descriptor memory must not evade the byte budget")

	c = newSnapshotReadCache(b, 2*weight-1)
	b.snapshotReads = c
	c.store(key, &record)
	require.Equal(t, weight, c.used)
	record.LocalCleanup.Exclude[0] = "caller mutation"
	first := c.lookup(key)
	require.Equal(t, []string{"cache", "tmp"}, first.LocalCleanup.Exclude)
	require.Equal(t, len(first.LocalCleanup.Exclude), cap(first.LocalCleanup.Exclude))
	first.LocalCleanup.Prefix = "changed/"
	first.LocalCleanup.Exclude[0] = "also changed"
	second := c.lookup(key)
	require.Equal(t, "team/project/", second.LocalCleanup.Prefix)
	require.Equal(t, []string{"cache", "tmp"}, second.LocalCleanup.Exclude)

	other := key
	other.sandboxID = "another"
	c.store(other, second)
	require.Nil(t, c.lookup(key), "second full-weight entry must evict the first")
	require.Equal(t, weight, c.used)
	require.NoError(t, b.Close())
	require.Zero(t, c.used)
	require.Empty(t, c.entries)
}

func TestLocalCleanupSnapshotRead(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	input := localCleanupTestInput(t, "read", []string{"cache"}, true)
	_, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	for _, entry := range []string{"workspace miss", "sandbox hit", "workspace hit"} {
		var result *SandboxSnapshotReadResult
		if strings.HasPrefix(entry, "workspace") {
			result, err = b.ReadWorkspaceSnapshot(ctx, input.Workspace)
		} else {
			result, err = b.ReadSandboxSnapshot(ctx, input.SandboxID)
		}
		require.NoError(t, err, entry)
		require.NotNil(t, result)
		require.NotNil(t, result.View)
		require.Equal(t, uint32(2), result.Snapshot.Version)
		require.NotNil(t, result.Snapshot.LocalCleanup)
		require.Equal(t, []string{"cache"}, result.Snapshot.LocalCleanup.Exclude)
		require.Equal(t, input.Workspace.Hash(), result.Snapshot.LocalCleanup.WorkspaceHash)
		result.Snapshot.LocalCleanup.Exclude[0] = "caller mutation"
	}
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	loaded, err := b.LoadSnapshot(ctx, input.Workspace.Partition(), input.SandboxID, control.Snapshot)
	require.NoError(t, err)
	require.Equal(t, []string{"cache"}, loaded.LocalCleanup.Exclude)

}

// Done is observed only after snapshotRecord has selected its original shared
// miss. The signal lets the test release a real RPC without timing-based sleeps.
type localCleanupWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *localCleanupWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestLocalCleanupSnapshotCoalescedMiss(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	input := localCleanupTestInput(t, "coalesced", []string{"cache"}, true)
	_, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
	require.NoError(t, err)
	placement, err := b.LoadPlacement(ctx, input.SandboxID)
	require.NoError(t, err)
	key := snapshotCacheKey{partition: input.Workspace.Partition(), sandboxID: input.SandboxID, version: control.Snapshot.Version, digest: control.Snapshot.Digest}
	arrived, release := make(chan struct{}), make(chan struct{})
	var unlock sync.Once
	resume := func() { unlock.Do(func() { close(release) }) }
	t.Cleanup(resume)
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		return len(ops) == 2 && ops[0].IsGet() && strings.Contains(string(ops[0].KeyBytes()), "/snapshots/")
	}, before: func() {
		close(arrived)
		<-release
	}}
	type answer struct {
		record *SandboxSnapshotRecord
		err    error
	}
	answers := make(chan answer, 2)
	for range 2 {
		call, err := b.registerSnapshotRead(ctx)
		require.NoError(t, err)
		waiting := &localCleanupWaitContext{Context: call.context, waiting: make(chan struct{})}
		call.context = waiting
		go func() {
			defer call.finish()
			record, err := b.snapshotRecord(call, key, *placement)
			answers <- answer{record: record, err: err}
		}()
		select {
		case <-waiting.waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("original read did not join its shared miss")
		}
	}
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("real snapshot RPC did not arrive")
	}
	resume()
	first, second := <-answers, <-answers
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotSame(t, first.record.LocalCleanup, second.record.LocalCleanup)
	first.record.LocalCleanup.Exclude[0] = "caller mutation"
	require.Equal(t, []string{"cache"}, second.record.LocalCleanup.Exclude)
	result, err := b.ReadWorkspaceSnapshot(ctx, input.Workspace)
	require.NoError(t, err)
	require.Equal(t, []string{"cache"}, result.Snapshot.LocalCleanup.Exclude)
}

func TestLocalCleanupSnapshotPayloadNull(t *testing.T) {
	record := localCleanupTestRecord(t)
	record.Payload = json.RawMessage(`null`)
	record.Snapshot.Digest = localCleanupTestDigest("sandbox.snapshot.v2", "2", "null", localCleanupTestJSON(t, record.LocalCleanup))
	wire, err := encodeDomainRecord(record)
	require.NoError(t, err, "opaque JSON null must remain a valid snapshot payload")
	require.Contains(t, wire, `"payload":null`)
	var decoded SandboxSnapshotRecord
	require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(wire)}, &decoded), "every valid encoded payload must remain readable")
	require.Equal(t, record, decoded)

	// Null is a present payload, not a missing field or an absent descriptor.
	for _, invalid := range []string{
		strings.Replace(wire, `,"payload":null`, "", 1),
		strings.Replace(wire, localCleanupTestJSON(t, record.LocalCleanup), "null", 1),
	} {
		before := decoded
		require.ErrorIs(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(invalid)}, &decoded), ErrCorruptRecord)
		require.Equal(t, before, decoded)
	}
	changed := record
	changed.Payload = json.RawMessage(`{}`)
	_, err = encodeDomainRecord(changed)
	require.Error(t, err, "null participates in the digest rather than meaning no payload")
}

// A coherent placement may change while another caller is reading the same
// immutable snapshot key. Build a separately valid same-partition identity so
// the rejection below depends on descriptor attribution, not malformed routing.
func localCleanupOtherPlacement(t *testing.T, original SandboxPlacementRecord) SandboxPlacementRecord {
	t.Helper()
	for i := 0; i < 65536; i++ {
		w, err := NewWorkspaceIdentity("local", "test-storage", "bucket", fmt.Sprintf("replacement/%d/", i))
		require.NoError(t, err)
		if w.Partition() != original.Partition || w.Hash() == original.WorkspaceHash {
			continue
		}
		other := original
		other.WorkspaceHash = w.Hash()
		require.NoError(t, other.Validate())
		return other
	}
	t.Fatal("bounded same-partition placement fixture was not found")
	return SandboxPlacementRecord{}
}

func TestLocalCleanupSnapshotCachePlacement(t *testing.T) {
	for _, mode := range []string{"cache hit", "coalesced miss"} {
		t.Run(mode, func(t *testing.T) {
			b, _ := integrationBackend(t)
			ctx := context.Background()
			input := localCleanupTestInput(t, "placement", []string{"cache"}, true)
			_, err := b.AcquireIntent(ctx, input)
			require.NoError(t, err)
			control, err := b.LoadControl(ctx, input.Workspace.Partition(), input.SandboxID)
			require.NoError(t, err)
			placement, err := b.LoadPlacement(ctx, input.SandboxID)
			require.NoError(t, err)
			require.NotNil(t, placement)
			other := localCleanupOtherPlacement(t, *placement)
			key := snapshotCacheKey{partition: placement.Partition, sandboxID: input.SandboxID, version: control.Snapshot.Version, digest: control.Snapshot.Digest}

			if mode == "cache hit" {
				warm, err := b.ReadWorkspaceSnapshot(ctx, input.Workspace)
				require.NoError(t, err)
				require.Equal(t, placement.WorkspaceHash, warm.Snapshot.LocalCleanup.WorkspaceHash)
				call, err := b.registerSnapshotRead(ctx)
				require.NoError(t, err)
				defer call.finish()
				got, err := b.snapshotRecord(call, key, other)
				require.ErrorIs(t, err, ErrCorruptRecord, "a digest cache hit still needs this caller's placement binding")
				require.Nil(t, got)
				return
			}

			// Hold only the actual point-read RPC. Both original reader calls
			// reach their shared miss before it is released, with no sleeps.
			arrived, release := make(chan struct{}), make(chan struct{})
			var unlock sync.Once
			resume := func() { unlock.Do(func() { close(release) }) }
			t.Cleanup(resume)
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				return len(ops) == 2 && ops[0].IsGet() && strings.Contains(string(ops[0].KeyBytes()), "/snapshots/")
			}, before: func() {
				close(arrived)
				<-release
			}}
			type answer struct {
				record *SandboxSnapshotRecord
				err    error
			}
			results := make([]chan answer, 2)
			for i, expected := range []SandboxPlacementRecord{*placement, other} {
				call, err := b.registerSnapshotRead(ctx)
				require.NoError(t, err)
				waiting := &localCleanupWaitContext{Context: call.context, waiting: make(chan struct{})}
				call.context = waiting
				done := make(chan answer, 1)
				results[i] = done
				go func() {
					defer call.finish()
					record, err := b.snapshotRecord(call, key, expected)
					done <- answer{record: record, err: err}
				}()
				select {
				case <-waiting.waiting:
				case <-time.After(5 * time.Second):
					t.Fatal("original caller did not join the held miss")
				}
			}
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Fatal("original snapshot point-read did not arrive")
			}
			resume()
			for i, done := range results {
				select {
				case got := <-done:
					if i == 0 {
						require.NoError(t, got.err)
						require.NotNil(t, got.record)
					} else {
						require.ErrorIs(t, got.err, ErrCorruptRecord, "a waiter cannot inherit the first reader's placement authentication")
						require.Nil(t, got.record)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("released snapshot reader did not join")
				}
			}
		})
	}
}

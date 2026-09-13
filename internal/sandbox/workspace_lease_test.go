package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
)

type atomicMemoryEntry struct {
	value     []byte
	expiresAt time.Time
}

type atomicMemoryStore struct {
	mu                           sync.Mutex
	entries                      map[string]atomicMemoryEntry
	increments                   map[string]int64
	failMethods                  map[string]error
	failSetNXAfterWrite          map[string]error
	failCASAfterWrite            map[string]error
	failVerifyAfterCAS           map[string]error
	casBlocks                    map[string]*atomicCASBlock
	casAfterWriteBlocks          map[string]*atomicCASBlock
	compareDeleteBlocks          map[string]*atomicCASBlock
	failCompareDeleteAfterWrite  map[string]error
	failCompareDeleteBeforeWrite map[string]error
	calls                        int
	compareAndSwapCalls          int
	failCompareAndSwapAt         int
	failCompareAndSwapErr        error
}

type atomicCASBlock struct {
	entered chan struct{}
	release chan struct{}
}

func newAtomicMemoryStore() *atomicMemoryStore {
	return &atomicMemoryStore{
		entries:                      make(map[string]atomicMemoryEntry),
		increments:                   make(map[string]int64),
		failMethods:                  make(map[string]error),
		failSetNXAfterWrite:          make(map[string]error),
		failCASAfterWrite:            make(map[string]error),
		failVerifyAfterCAS:           make(map[string]error),
		casBlocks:                    make(map[string]*atomicCASBlock),
		casAfterWriteBlocks:          make(map[string]*atomicCASBlock),
		compareDeleteBlocks:          make(map[string]*atomicCASBlock),
		failCompareDeleteAfterWrite:  make(map[string]error),
		failCompareDeleteBeforeWrite: make(map[string]error),
	}
}

func (s *atomicMemoryStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Set"); err != nil {
		return err
	}
	s.setLocked(key, value, ttl)
	return nil
}

func (s *atomicMemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Get"); err != nil {
		return nil, err
	}
	entry, ok := s.getLocked(key)
	if !ok {
		return nil, nil
	}
	return append([]byte(nil), entry.value...), nil
}

func (s *atomicMemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Delete"); err != nil {
		return err
	}
	delete(s.entries, key)
	return nil
}

func (s *atomicMemoryStore) Exists(_ context.Context, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Exists"); err != nil {
		return false, err
	}
	_, ok := s.getLocked(key)
	return ok, nil
}

func (s *atomicMemoryStore) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("SetNX"); err != nil {
		return false, err
	}
	if _, ok := s.getLocked(key); ok {
		return false, nil
	}
	s.setLocked(key, value, ttl)
	if err := s.failSetNXAfterWrite[key]; err != nil {
		delete(s.failSetNXAfterWrite, key)
		return false, err
	}
	return true, nil
}

func (s *atomicMemoryStore) Keys(_ context.Context, pattern string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Keys"); err != nil {
		return nil, err
	}
	keys := make([]string, 0)
	for key := range s.entries {
		if matched, _ := path.Match(pattern, key); matched {
			if _, ok := s.getLocked(key); ok {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *atomicMemoryStore) CompareAndSwap(_ context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	if block := s.casBlocks[key]; block != nil {
		delete(s.casBlocks, key)
		s.mu.Unlock()
		close(block.entered)
		<-block.release
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	s.calls++
	s.compareAndSwapCalls++
	if s.compareAndSwapCalls == s.failCompareAndSwapAt {
		return false, s.failCompareAndSwapErr
	}
	if err := s.takeFailure("CompareAndSwap"); err != nil {
		return false, err
	}
	entry, ok := s.getLocked(key)
	if !ok || string(entry.value) != string(oldValue) {
		return false, nil
	}
	s.setLocked(key, newValue, ttl)
	if block := s.casAfterWriteBlocks[key]; block != nil {
		delete(s.casAfterWriteBlocks, key)
		s.mu.Unlock()
		close(block.entered)
		<-block.release
		s.mu.Lock()
	}
	if err := s.failCASAfterWrite[key]; err != nil {
		delete(s.failCASAfterWrite, key)
		if verifyErr := s.failVerifyAfterCAS[key]; verifyErr != nil {
			delete(s.failVerifyAfterCAS, key)
			s.failMethods["Get"] = verifyErr
		}
		return false, err
	}
	return true, nil
}

func (s *atomicMemoryStore) CompareAndDelete(_ context.Context, key string, expected []byte) (bool, error) {
	s.mu.Lock()
	if block := s.compareDeleteBlocks[key]; block != nil {
		delete(s.compareDeleteBlocks, key)
		s.mu.Unlock()
		close(block.entered)
		<-block.release
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	s.calls++
	if err := s.failCompareDeleteBeforeWrite[key]; err != nil {
		delete(s.failCompareDeleteBeforeWrite, key)
		return false, err
	}
	if err := s.takeFailure("CompareAndDelete"); err != nil {
		return false, err
	}
	entry, ok := s.getLocked(key)
	if !ok || string(entry.value) != string(expected) {
		return false, nil
	}
	delete(s.entries, key)
	if err := s.failCompareDeleteAfterWrite[key]; err != nil {
		delete(s.failCompareDeleteAfterWrite, key)
		return false, err
	}
	return true, nil
}

func (s *atomicMemoryStore) Increment(_ context.Context, key string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if err := s.takeFailure("Increment"); err != nil {
		return 0, err
	}
	if s.increments[key] == math.MaxInt64 {
		return 0, state.ErrIncrementOverflow
	}
	s.increments[key]++
	return s.increments[key], nil
}

func (s *atomicMemoryStore) setLocked(key string, value []byte, ttl time.Duration) {
	entry := atomicMemoryEntry{value: append([]byte(nil), value...)}
	if ttl > 0 {
		entry.expiresAt = time.Now().Add(ttl)
	}
	s.entries[key] = entry
}

func (s *atomicMemoryStore) getLocked(key string) (atomicMemoryEntry, bool) {
	entry, ok := s.entries[key]
	if ok && !entry.expiresAt.IsZero() && !time.Now().Before(entry.expiresAt) {
		delete(s.entries, key)
		return atomicMemoryEntry{}, false
	}
	return entry, ok
}

func (s *atomicMemoryStore) takeFailure(method string) error {
	err := s.failMethods[method]
	delete(s.failMethods, method)
	return err
}

func (s *atomicMemoryStore) failNext(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMethods[method] = err
}

func (s *atomicMemoryStore) failSetNXAfterWriting(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSetNXAfterWrite[key] = err
}

func (s *atomicMemoryStore) failCompareAndDeleteAfterWriting(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCompareDeleteAfterWrite[key] = err
}

func (s *atomicMemoryStore) failCompareAndDeleteBeforeWriting(key string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCompareDeleteBeforeWrite[key] = err
}

func (s *atomicMemoryStore) failCompareAndSwapCall(call int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCompareAndSwapAt = call
	s.failCompareAndSwapErr = err
}

func (s *atomicMemoryStore) failCompareAndSwapAfterWriting(key string, writeErr, verifyErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCASAfterWrite[key] = writeErr
	if verifyErr != nil {
		s.failVerifyAfterCAS[key] = verifyErr
	}
}

func (s *atomicMemoryStore) blockNextCompareAndSwap(key string) *atomicCASBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := &atomicCASBlock{entered: make(chan struct{}), release: make(chan struct{})}
	s.casBlocks[key] = block
	return block
}

func (s *atomicMemoryStore) blockNextCompareAndDelete(key string) *atomicCASBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := &atomicCASBlock{entered: make(chan struct{}), release: make(chan struct{})}
	s.compareDeleteBlocks[key] = block
	return block
}

func (s *atomicMemoryStore) blockNextCompareAndSwapAfterWrite(key string) *atomicCASBlock {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := &atomicCASBlock{entered: make(chan struct{}), release: make(chan struct{})}
	s.casAfterWriteBlocks[key] = block
	return block
}

func (s *atomicMemoryStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *atomicMemoryStore) setGeneration(key string, generation int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.increments[key] = generation
}

func (s *atomicMemoryStore) putOwner(req WorkspaceLeaseRequest, owner WorkspaceOwner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := workspaceStateKeys(req)
	if err != nil {
		panic(err)
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		panic(err)
	}
	s.setLocked(keys.owner, raw, 0)
}

func (s *atomicMemoryStore) replaceLeaseValue(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(key, value, time.Minute)
}

func (s *atomicMemoryStore) deleteKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

func (s *atomicMemoryStore) expireKey(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	s.entries[key] = entry
}

func (s *atomicMemoryStore) hasKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.getLocked(key)
	return ok
}

func validLeaseRequest() WorkspaceLeaseRequest {
	return WorkspaceLeaseRequest{
		MountType:       WorkspaceMountFUSE,
		Provider:        "minio",
		StorageIdentity: "minio-primary",
		Bucket:          "sandbox",
		Prefix:          "workspaces/a/",
		SandboxID:       "sandbox-a",
		Runtime:         "docker",
		RuntimeID:       "container-a",
	}
}

func TestWorkspaceCoordinatorUsesOneLeaseAcrossSyncAndFUSE(t *testing.T) {
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	fuseRequest := validLeaseRequest()
	fuseLease, err := coordinator.Acquire(context.Background(), fuseRequest)
	require.NoError(t, err)

	syncRequest := fuseRequest
	syncRequest.MountType = WorkspaceMountSync
	syncRequest.SandboxID = "sandbox-sync"
	syncRequest.RuntimeID = "container-sync"
	_, err = coordinator.Acquire(context.Background(), syncRequest)
	require.ErrorIs(t, err, ErrWorkspaceLeased)

	fuseKeys, err := workspaceStateKeys(fuseRequest)
	require.NoError(t, err)
	syncKeys, err := workspaceStateKeys(syncRequest)
	require.NoError(t, err)
	assert.Equal(t, fuseKeys, syncKeys)
	assert.Equal(t, WorkspaceMountFUSE, fuseLease.OwnerSnapshot().MountType)
}

func TestWorkspaceCoordinatorAllowsSyncReleaseWithoutRuntimeTermination(t *testing.T) {
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.MountType = WorkspaceMountSync
	req.RuntimeUID = "uid-sync"
	lease, err := coordinator.Acquire(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, coordinator.Release(context.Background(), lease, runtime.TerminationEvidence{}))
	assert.False(t, store.hasKey(lease.Key))
}

func TestWorkspaceCoordinatorKeepsFUSETerminationEvidenceRequirement(t *testing.T) {
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-fuse"
	lease, err := coordinator.Acquire(context.Background(), req)
	require.NoError(t, err)

	err = coordinator.Release(context.Background(), lease, runtime.TerminationEvidence{})
	require.ErrorIs(t, err, ErrRuntimeExitUnconfirmed)
}

func TestWorkspaceCoordinatorPublishesFinalLeaseRecord(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	var record workspaceLeaseRecord
	require.NoError(t, json.Unmarshal(lease.Value, &record))
	assert.Equal(t, workspaceLeasePhaseActive, record.Phase)
	assert.Equal(t, req.SandboxID, record.SandboxID)
	assert.Equal(t, req.Runtime, record.Runtime)
	assert.Equal(t, req.RuntimeID, record.RuntimeID)
	assert.Equal(t, req.RuntimeUID, record.RuntimeUID)
	assert.Equal(t, lease.Owner.Generation, record.Generation)
	assert.NotEmpty(t, record.Token)
	assert.False(t, record.CreatedAt.IsZero())
	stored, getErr := store.Get(context.Background(), lease.Key)
	require.NoError(t, getErr)
	assert.Equal(t, lease.Value, stored)
}

func TestWorkspaceCoordinatorRenewAndReleaseRejectInvalidLeaseRecords(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, lease *WorkspaceLease) []byte
	}{
		{name: "provisional", mutate: func(t *testing.T, lease *WorkspaceLease) []byte {
			t.Helper()
			var record workspaceLeaseRecord
			require.NoError(t, json.Unmarshal(lease.Value, &record))
			record.Phase = workspaceLeasePhaseProvisional
			record.Generation = 0
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			return raw
		}},
		{name: "malformed", mutate: func(t *testing.T, _ *WorkspaceLease) []byte {
			t.Helper()
			return []byte(`{"phase":"active","token":`)
		}},
		{name: "identity mismatch", mutate: func(t *testing.T, lease *WorkspaceLease) []byte {
			t.Helper()
			var record workspaceLeaseRecord
			require.NoError(t, json.Unmarshal(lease.Value, &record))
			record.WorkspaceHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			return raw
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" renew", func(t *testing.T) {
			store := newAtomicMemoryStore()
			c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
			lease, err := c.Acquire(context.Background(), validLeaseRequest())
			require.NoError(t, err)
			forged := cloneWorkspaceLeaseWithValue(lease, tt.mutate(t, lease))
			err = c.Renew(context.Background(), forged)
			require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
		})
		t.Run(tt.name+" release", func(t *testing.T) {
			store := newAtomicMemoryStore()
			c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
			lease, err := c.Acquire(context.Background(), validLeaseRequest())
			require.NoError(t, err)
			forged := cloneWorkspaceLeaseWithValue(lease, tt.mutate(t, lease))
			err = c.Release(context.Background(), forged, runtime.TerminationEvidence{})
			require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
			assert.True(t, store.hasKey(lease.Key))
		})
	}
}

func TestWorkspaceCoordinatorStrictLeaseJSON(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "unknown field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"unknown":1`) }},
		{name: "case variant field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"Phase":"active"`) }},
		{name: "duplicate field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"phase":"active"`) }},
		{name: "missing runtime UID", mutate: func(raw []byte) []byte { return removeJSONObjectField(raw, "runtime_uid") }},
		{name: "second value", mutate: func(raw []byte) []byte { return append(append([]byte(nil), raw...), []byte(` {}`)...) }},
	}
	operations := []struct {
		name string
		run  func(*WorkspaceCoordinator, *WorkspaceLease) error
	}{
		{name: "renew", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.Renew(context.Background(), lease)
		}},
		{name: "bind", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.BindRuntime(context.Background(), lease, "uid-a")
		}},
		{name: "consume", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			_, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
			return err
		}},
		{name: "release", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.Release(context.Background(), lease, runtime.TerminationEvidence{})
		}},
	}
	for _, mutation := range mutations {
		for _, operation := range operations {
			t.Run(mutation.name+"/"+operation.name, func(t *testing.T) {
				store := newAtomicMemoryStore()
				c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
				lease, err := c.Acquire(context.Background(), validLeaseRequest())
				require.NoError(t, err)
				storedBefore, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				ownerBefore, getErr := store.Get(context.Background(), lease.ownerKey)
				require.NoError(t, getErr)
				forged := cloneWorkspaceLeaseWithValue(lease, mutation.mutate(lease.Value))

				err = operation.run(c, forged)
				require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
				storedAfter, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				assert.Equal(t, storedBefore, storedAfter)
				ownerAfter, getErr := store.Get(context.Background(), lease.ownerKey)
				require.NoError(t, getErr)
				assert.Equal(t, ownerBefore, ownerAfter)
			})
		}
	}

	t.Run("trailing whitespace accepted", func(t *testing.T) {
		store := newAtomicMemoryStore()
		c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		lease, err := c.Acquire(context.Background(), validLeaseRequest())
		require.NoError(t, err)
		withWhitespace := append(append([]byte(nil), lease.Value...), []byte(" \n\t")...)
		store.replaceLeaseValue(lease.Key, withWhitespace)
		forged := cloneWorkspaceLeaseWithValue(lease, withWhitespace)
		require.NoError(t, c.Renew(context.Background(), forged))
	})
}

func TestWorkspaceCoordinatorStrictOwnerJSON(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "unknown field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"unknown":1`) }},
		{name: "case variant field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"RuntimeUID":"uid-a"`) }},
		{name: "duplicate field", mutate: func(raw []byte) []byte { return appendBeforeJSONObjectEnd(raw, `,"mount_attempt":0`) }},
		{name: "null runtime UID", mutate: func(raw []byte) []byte { return replaceJSONObjectField(raw, "runtime_uid", json.RawMessage("null")) }},
		{name: "missing mount attempt", mutate: func(raw []byte) []byte { return removeJSONObjectField(raw, "mount_attempt") }},
		{name: "missing runtime UID", mutate: func(raw []byte) []byte { return removeJSONObjectField(raw, "runtime_uid") }},
		{name: "second value", mutate: func(raw []byte) []byte { return append(append([]byte(nil), raw...), []byte(` {}`)...) }},
	}
	operations := []struct {
		name  string
		bound bool
		run   func(*WorkspaceCoordinator, *WorkspaceLease) error
	}{
		{name: "bind", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.BindRuntime(context.Background(), lease, "uid-a")
		}},
		{name: "consume", bound: true, run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			_, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
			return err
		}},
		{name: "release", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.Release(context.Background(), lease, runtime.TerminationEvidence{})
		}},
	}
	for _, mutation := range mutations {
		for _, operation := range operations {
			t.Run(mutation.name+"/"+operation.name, func(t *testing.T) {
				store := newAtomicMemoryStore()
				c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
				req := validLeaseRequest()
				if operation.bound {
					req.RuntimeUID = "uid-a"
				}
				lease, err := c.Acquire(context.Background(), req)
				require.NoError(t, err)
				keys, keyErr := workspaceStateKeys(req)
				require.NoError(t, keyErr)
				ownerBefore, getErr := store.Get(context.Background(), keys.owner)
				require.NoError(t, getErr)
				leaseBefore, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				corrupt := mutation.mutate(ownerBefore)
				require.NoError(t, store.Set(context.Background(), keys.owner, corrupt, 0))

				err = operation.run(c, lease)
				require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
				ownerAfter, getErr := store.Get(context.Background(), keys.owner)
				require.NoError(t, getErr)
				assert.Equal(t, corrupt, ownerAfter)
				leaseAfter, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				assert.Equal(t, leaseBefore, leaseAfter)
			})
		}
	}

	t.Run("trailing whitespace accepted", func(t *testing.T) {
		store := newAtomicMemoryStore()
		c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		req := validLeaseRequest()
		req.RuntimeUID = "uid-a"
		lease, err := c.Acquire(context.Background(), req)
		require.NoError(t, err)
		keys, keyErr := workspaceStateKeys(req)
		require.NoError(t, keyErr)
		owner, getErr := store.Get(context.Background(), keys.owner)
		require.NoError(t, getErr)
		require.NoError(t, store.Set(context.Background(), keys.owner, append(owner, []byte(" \n\t")...), 0))
		_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
		require.NoError(t, err)
	})
}

func appendBeforeJSONObjectEnd(raw []byte, suffix string) []byte {
	result := append([]byte(nil), raw[:len(raw)-1]...)
	result = append(result, suffix...)
	return append(result, '}')
}

func removeJSONObjectField(raw []byte, field string) []byte {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		panic(err)
	}
	delete(object, field)
	result, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return result
}

func replaceJSONObjectField(raw []byte, field string, value json.RawMessage) []byte {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		panic(err)
	}
	object[field] = append(json.RawMessage(nil), value...)
	result, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return result
}

func TestWorkspaceCoordinatorStrictLeaseJSONRejectsNullOptionalRuntimeFields(t *testing.T) {
	fields := []string{"runtime", "runtime_id", "runtime_uid"}
	operations := []struct {
		name string
		run  func(*WorkspaceCoordinator, *WorkspaceLease) error
	}{
		{name: "renew", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.Renew(context.Background(), lease)
		}},
		{name: "bind", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.BindRuntime(context.Background(), lease, "uid-a")
		}},
		{name: "release", run: func(c *WorkspaceCoordinator, lease *WorkspaceLease) error {
			return c.Release(context.Background(), lease, runtime.TerminationEvidence{})
		}},
	}
	for _, field := range fields {
		for _, operation := range operations {
			t.Run(field+"/"+operation.name, func(t *testing.T) {
				store := newAtomicMemoryStore()
				c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
				req := validLeaseRequest()
				req.Runtime = ""
				req.RuntimeID = ""
				req.RuntimeUID = ""
				lease, err := c.Acquire(context.Background(), req)
				require.NoError(t, err)
				leaseBefore, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				ownerBefore, getErr := store.Get(context.Background(), lease.ownerKey)
				require.NoError(t, getErr)
				forged := cloneWorkspaceLeaseWithValue(lease, replaceJSONObjectField(lease.Value, field, json.RawMessage("null")))

				err = operation.run(c, forged)
				require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
				assert.NotContains(t, err.Error(), "null")
				leaseAfter, getErr := store.Get(context.Background(), lease.Key)
				require.NoError(t, getErr)
				ownerAfter, getErr := store.Get(context.Background(), lease.ownerKey)
				require.NoError(t, getErr)
				assert.Equal(t, leaseBefore, leaseAfter)
				assert.Equal(t, ownerBefore, ownerAfter)
			})
		}
	}
}

func cloneWorkspaceLeaseWithValue(lease *WorkspaceLease, value []byte) *WorkspaceLease {
	return &WorkspaceLease{
		Key:             lease.Key,
		Value:           append([]byte(nil), value...),
		Prefix:          lease.Prefix,
		WorkspaceHash:   lease.WorkspaceHash,
		Owner:           lease.Owner,
		ExpiresAt:       lease.ExpiresAt,
		ownerKey:        lease.ownerKey,
		generationKey:   lease.generationKey,
		boundRuntimeUID: lease.boundRuntimeUID,
	}
}

func TestWorkspaceCoordinatorConsumesMountAttemptOnce(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, 90*time.Second, 30*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	assert.Equal(t, runtime.WorkspaceMountAuthorization{
		RuntimeUID:      "uid-a",
		PoolKey:         "pool-key",
		WorkspaceHash:   lease.WorkspaceHash,
		Prefix:          "workspaces/a/",
		LeaseGeneration: 1,
		MountAttempt:    1,
	}, auth)
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrMountAuthorizationConsumed)
}

func TestWorkspaceCoordinatorDoesNotTakeOverExpiredLeaseOwner(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Second, 300*time.Millisecond)
	req := validLeaseRequest()
	store.putOwner(req, WorkspaceOwner{
		Provider:            req.Provider,
		StorageIdentityHash: storageIdentityHash(req.StorageIdentity),
		Bucket:              req.Bucket,
		Prefix:              req.Prefix,
		WorkspaceHash:       mustWorkspaceHash(t, req),
		SandboxID:           "old-sandbox",
		RuntimeUID:          "live-uid",
		Generation:          7,
		UpdatedAt:           time.Now().UTC(),
	})

	_, err := c.Acquire(context.Background(), req)
	require.ErrorIs(t, err, ErrWorkspaceOwned)
	keys, keyErr := workspaceStateKeys(req)
	require.NoError(t, keyErr)
	assert.True(t, store.hasKey(keys.owner))
	assert.False(t, store.hasKey(keys.lease), "failed acquire must release only its own lease")
	assert.Zero(t, store.increments[keys.generation], "blocked takeovers must not consume generations")
}

func TestWorkspaceCoordinatorRejectsOwnerRuntimeChangedOutsideBind(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	owner := lease.Owner
	owner.RuntimeUID = "injected-uid"
	owner.UpdatedAt = time.Now().UTC()
	raw, marshalErr := json.Marshal(owner)
	require.NoError(t, marshalErr)
	require.NoError(t, store.Set(context.Background(), keys.owner, raw, 0))

	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
}

func TestWorkspaceCoordinatorBindsOnlyEmptyRuntime(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	leaseValueBeforeBind := append([]byte(nil), lease.Value...)

	require.NoError(t, c.BindRuntime(context.Background(), lease, "uid-a"))
	var record workspaceLeaseRecord
	require.NoError(t, json.Unmarshal(lease.Value, &record))
	assert.Empty(t, record.RuntimeUID, "lease keeps its immutable acquire-time identity snapshot")
	storedLease, getErr := store.Get(context.Background(), lease.Key)
	require.NoError(t, getErr)
	assert.Equal(t, leaseValueBeforeBind, storedLease)
	err = c.BindRuntime(context.Background(), lease, "uid-b")
	require.ErrorIs(t, err, ErrWorkspaceRuntimeBound)

	auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	assert.Equal(t, "uid-a", auth.RuntimeUID)
}

func TestWorkspaceCoordinatorBindConfirmsWriteAfterError(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	leaseBefore := append([]byte(nil), lease.Value...)
	store.failCompareAndSwapAfterWriting(keys.owner, errors.New("reply lost"), nil)

	require.NoError(t, c.BindRuntime(context.Background(), lease, "uid-a"))
	assert.Equal(t, leaseBefore, lease.Value)
	auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	assert.Equal(t, "uid-a", auth.RuntimeUID)
}

func TestWorkspaceCoordinatorBindVerificationFailureKeepsFailClosedOwner(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	leaseBefore := append([]byte(nil), lease.Value...)
	store.failCompareAndSwapAfterWriting(keys.owner, errors.New("reply lost"), errors.New("verification unavailable"))

	err = c.BindRuntime(context.Background(), lease, "uid-a")
	require.Error(t, err)
	assert.Equal(t, leaseBefore, lease.Value)
	ownerBefore, getErr := store.Get(context.Background(), keys.owner)
	require.NoError(t, getErr)
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
	require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
	ownerAfter, getErr := store.Get(context.Background(), keys.owner)
	require.NoError(t, getErr)
	assert.Equal(t, ownerBefore, ownerAfter)
	assert.True(t, store.hasKey(keys.lease))
}

func TestWorkspaceCoordinatorBindFailsClosedWhenLeaseExpiresDuringOwnerCAS(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	block := store.blockNextCompareAndSwap(keys.owner)

	bindResult := make(chan error, 1)
	go func() {
		bindResult <- c.BindRuntime(context.Background(), lease, "uid-a")
	}()
	<-block.entered
	store.expireKey(lease.Key)
	close(block.release)

	require.ErrorIs(t, <-bindResult, ErrWorkspaceLeaseLost)
	ownerRaw, getErr := store.Get(context.Background(), keys.owner)
	require.NoError(t, getErr)
	var owner WorkspaceOwner
	require.NoError(t, json.Unmarshal(ownerRaw, &owner))
	assert.Equal(t, "uid-a", owner.RuntimeUID, "confirmed binding remains as a persistent fail-closed fence")
	assert.False(t, store.hasKey(lease.Key))
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
}

func TestWorkspaceCoordinatorBindFailsClosedWhenLeaseExpiresAfterAmbiguousOwnerCAS(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	store.failCompareAndSwapAfterWriting(keys.owner, errors.New("reply lost"), nil)
	block := store.blockNextCompareAndSwapAfterWrite(keys.owner)

	bindResult := make(chan error, 1)
	go func() {
		bindResult <- c.BindRuntime(context.Background(), lease, "uid-a")
	}()
	<-block.entered
	store.expireKey(lease.Key)
	close(block.release)

	require.ErrorIs(t, <-bindResult, ErrWorkspaceLeaseLost)
	ownerRaw, getErr := store.Get(context.Background(), keys.owner)
	require.NoError(t, getErr)
	var owner WorkspaceOwner
	require.NoError(t, json.Unmarshal(ownerRaw, &owner))
	assert.Equal(t, "uid-a", owner.RuntimeUID)
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
}

func TestWorkspaceCoordinatorAcquireWithRuntimeUIDRequiresThatExactRuntime(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "known-uid"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	err = c.BindRuntime(context.Background(), lease, "other-uid")
	require.ErrorIs(t, err, ErrWorkspaceRuntimeBound)
	auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	assert.Equal(t, "known-uid", auth.RuntimeUID)
}

func TestWorkspaceCoordinatorConcurrentAcquireHasOneWinner(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	const contenders = 20
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Acquire(context.Background(), validLeaseRequest())
			if err == nil {
				successes.Add(1)
				return
			}
			assert.ErrorIs(t, err, ErrWorkspaceLeased)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), successes.Load())
}

func TestWorkspaceCoordinatorConcurrentMountAttemptHasOneWinner(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	const contenders = 20
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, consumeErr := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
			if consumeErr == nil {
				successes.Add(1)
				return
			}
			assert.ErrorIs(t, consumeErr, ErrMountAuthorizationConsumed)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), successes.Load())
}

func TestWorkspaceCoordinatorNullMountAttemptCannotReplayConsumedAuthorization(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)

	consumedOwner, getErr := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, getErr)
	corruptOwner := replaceJSONObjectField(consumedOwner, "mount_attempt", json.RawMessage("null"))
	require.NoError(t, store.Set(context.Background(), lease.ownerKey, corruptOwner, 0))
	_, err = c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.ErrorIs(t, err, ErrWorkspaceOwnerLost)
	assert.NotContains(t, err.Error(), "null")
	ownerAfter, getErr := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, getErr)
	assert.Equal(t, corruptOwner, ownerAfter)
}

func TestWorkspaceCoordinatorReleaseWaitsForInFlightMountAttempt(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)
	keys, keyErr := workspaceStateKeys(req)
	require.NoError(t, keyErr)
	consumeBlock := store.blockNextCompareAndSwap(keys.owner)
	releaseDeleteBlock := store.blockNextCompareAndDelete(lease.Key)

	authResult := make(chan error, 1)
	go func() {
		_, consumeErr := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
		authResult <- consumeErr
	}()
	<-consumeBlock.entered
	if lease.runtimeMu.TryRLock() {
		lease.runtimeMu.RUnlock()
		t.Fatal("mount authorization did not hold the exclusive lease lock")
	}
	releaseResult := make(chan error, 1)
	releaseStarted := make(chan struct{})
	go func() {
		close(releaseStarted)
		releaseResult <- c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
	}()
	<-releaseStarted
	close(consumeBlock.release)
	require.NoError(t, <-authResult)
	<-releaseDeleteBlock.entered
	if lease.runtimeMu.TryRLock() {
		lease.runtimeMu.RUnlock()
		t.Fatal("release did not hold the exclusive lease lock")
	}
	assert.True(t, store.hasKey(lease.Key), "release must not delete the lease before authorization returns")
	close(releaseDeleteBlock.release)
	require.NoError(t, <-releaseResult)
}

func TestWorkspaceCoordinatorRenewNeverOverwritesAnotherLease(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	store.replaceLeaseValue(lease.Key, []byte("other-owner-secret"))

	err = c.Renew(context.Background(), lease)
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
	got, getErr := store.Get(context.Background(), lease.Key)
	require.NoError(t, getErr)
	assert.Equal(t, []byte("other-owner-secret"), got)
}

func TestWorkspaceCoordinatorReleaseRequiresExactExitEvidence(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "uid-a"})
	require.ErrorIs(t, err, ErrRuntimeExitUnconfirmed)
	assert.True(t, store.hasKey(lease.Key))

	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "wrong", ProcessExited: true})
	require.ErrorIs(t, err, ErrRuntimeExitUnconfirmed)
	assert.True(t, store.hasKey(lease.Key))

	require.NoError(t, c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true}))
	assert.False(t, store.hasKey(lease.Key))
	keys, keyErr := workspaceStateKeys(req)
	require.NoError(t, keyErr)
	assert.False(t, store.hasKey(keys.owner))
	assert.Equal(t, int64(1), store.increments[keys.generation], "generation is persistent")
}

func TestWorkspaceCoordinatorKubernetesReleaseRequiresUnmountAndExitOrFencing(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.Runtime = "kubernetes"
	req.RuntimeUID = "pod-uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)

	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "pod-uid-a", ProcessExited: true})
	require.ErrorIs(t, err, ErrRuntimeExitUnconfirmed)
	require.NoError(t, c.Release(context.Background(), lease, runtime.TerminationEvidence{
		RuntimeUID:      "pod-uid-a",
		GracefulUnmount: true,
		ProcessExited:   true,
	}))
}

func TestWorkspaceCoordinatorReleaseWithoutBoundRuntimeNeedsNoFabricatedEvidence(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	require.NoError(t, c.Release(context.Background(), lease, runtime.TerminationEvidence{}))
}

func TestWorkspaceCoordinatorReleaseRetriesOwnerPhaseWithoutLosingLease(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	keys, err := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, err)
	store.failCompareAndDeleteBeforeWriting(keys.owner, errors.New("owner delete unavailable"))

	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{})
	require.ErrorContains(t, err, "owner delete unavailable")
	assert.True(t, store.hasKey(keys.owner))
	assert.True(t, store.hasKey(keys.lease), "lease must keep renewal authority until owner deletion succeeds")

	require.NoError(t, c.Release(context.Background(), lease, runtime.TerminationEvidence{}))
	assert.False(t, store.hasKey(keys.owner))
	assert.False(t, store.hasKey(keys.lease))
}

func TestWorkspaceCoordinatorReleaseNeverDeletesForeignState(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.RuntimeUID = "uid-a"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)
	store.replaceLeaseValue(lease.Key, []byte("other-owner-secret"))

	err = c.Release(context.Background(), lease, runtime.TerminationEvidence{RuntimeUID: "uid-a", ProcessExited: true})
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
	keys, keyErr := workspaceStateKeys(req)
	require.NoError(t, keyErr)
	assert.True(t, store.hasKey(keys.owner))
	got, getErr := store.Get(context.Background(), lease.Key)
	require.NoError(t, getErr)
	assert.Equal(t, []byte("other-owner-secret"), got)
}

func TestWorkspaceCoordinatorGenerationStrictlyIncreasesAfterRelease(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	first, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	require.NoError(t, c.Release(context.Background(), first, runtime.TerminationEvidence{}))
	second, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	assert.Equal(t, first.Owner.Generation+1, second.Owner.Generation)
}

func TestWorkspaceCoordinatorCompensatesFailedGeneration(t *testing.T) {
	store := newAtomicMemoryStore()
	store.failNext("Increment", errors.New("redis unavailable"))
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	_, err := c.Acquire(context.Background(), validLeaseRequest())
	require.Error(t, err)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	assert.False(t, store.hasKey(keys.lease))
	assert.False(t, store.hasKey(keys.owner))
}

func TestWorkspaceCoordinatorCompensatesAmbiguousOwnerCreate(t *testing.T) {
	store := newAtomicMemoryStore()
	keys, err := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, err)
	store.failSetNXAfterWriting(keys.owner, errors.New("connection lost after write"))
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	_, err = c.Acquire(context.Background(), validLeaseRequest())
	require.Error(t, err)
	assert.False(t, store.hasKey(keys.lease))
	assert.False(t, store.hasKey(keys.owner), "exact owner written by the failed attempt must be compensated")
}

func TestWorkspaceCoordinatorCompensatesAmbiguousProvisionalLeaseCreate(t *testing.T) {
	store := newAtomicMemoryStore()
	keys, err := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, err)
	store.failSetNXAfterWriting(keys.lease, errors.New("connection lost after write"))
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	_, err = c.Acquire(context.Background(), validLeaseRequest())
	require.Error(t, err)
	assert.False(t, store.hasKey(keys.lease))
	assert.False(t, store.hasKey(keys.owner))
}

func TestWorkspaceCoordinatorCompensatesAmbiguousActiveLeasePublication(t *testing.T) {
	store := newAtomicMemoryStore()
	keys, err := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, err)
	store.failCompareAndSwapAfterWriting(keys.lease, errors.New("connection lost after write"), nil)
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)

	_, err = c.Acquire(context.Background(), validLeaseRequest())
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
	assert.False(t, store.hasKey(keys.lease), "exact active lease written by the failed publication must be compensated")
	assert.False(t, store.hasKey(keys.owner))
	assert.Equal(t, int64(1), store.increments[keys.generation], "fencing generations are never rolled back")
}

func TestWorkspaceCoordinatorGenerationOverflowLeavesNoLeaseOrOwner(t *testing.T) {
	store := newAtomicMemoryStore()
	keys, err := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, err)
	store.setGeneration(keys.generation, math.MaxInt64)
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	_, err = c.Acquire(context.Background(), validLeaseRequest())
	require.ErrorIs(t, err, ErrWorkspaceGenerationExhausted)
	assert.False(t, store.hasKey(keys.lease))
	assert.False(t, store.hasKey(keys.owner))
}

func TestWorkspaceCoordinatorVerifiesLeaseAfterOwnerPublication(t *testing.T) {
	store := newAtomicMemoryStore()
	store.failCompareAndSwapCall(2, errors.New("lease verification unavailable"))
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	_, err := c.Acquire(context.Background(), validLeaseRequest())
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
	keys, keyErr := workspaceStateKeys(validLeaseRequest())
	require.NoError(t, keyErr)
	assert.False(t, store.hasKey(keys.lease))
	assert.False(t, store.hasKey(keys.owner))
}

func TestWorkspaceCoordinatorRejectsInvalidConfigurationAndInputsWithoutLeakingThem(t *testing.T) {
	tests := []struct {
		name string
		c    *WorkspaceCoordinator
		req  WorkspaceLeaseRequest
	}{
		{name: "zero ttl", c: NewWorkspaceCoordinator(newAtomicMemoryStore(), 0, time.Second), req: validLeaseRequest()},
		{name: "renew too slow", c: NewWorkspaceCoordinator(newAtomicMemoryStore(), time.Minute, 21*time.Second), req: validLeaseRequest()},
		{name: "non canonical prefix", c: NewWorkspaceCoordinator(newAtomicMemoryStore(), time.Minute, 10*time.Second), req: func() WorkspaceLeaseRequest {
			r := validLeaseRequest()
			r.Prefix = "workspaces//secret-value/"
			return r
		}()},
		{name: "control in bucket", c: NewWorkspaceCoordinator(newAtomicMemoryStore(), time.Minute, 10*time.Second), req: func() WorkspaceLeaseRequest { r := validLeaseRequest(); r.Bucket = "secret-value\x00"; return r }()},
		{name: "empty sandbox", c: NewWorkspaceCoordinator(newAtomicMemoryStore(), time.Minute, 10*time.Second), req: func() WorkspaceLeaseRequest { r := validLeaseRequest(); r.SandboxID = ""; return r }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.c.Acquire(context.Background(), tt.req)
			require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
			assert.NotContains(t, err.Error(), "secret-value")
		})
	}
}

func TestWorkspaceCoordinatorRejectsOversizedInputsBeforeStoreAccess(t *testing.T) {
	tooLong := strings.Repeat("x", workspaceLeaseMaxFieldBytes+1)
	tests := []struct {
		name   string
		mutate func(*WorkspaceLeaseRequest)
	}{
		{name: "provider", mutate: func(req *WorkspaceLeaseRequest) { req.Provider = tooLong }},
		{name: "storage identity", mutate: func(req *WorkspaceLeaseRequest) { req.StorageIdentity = tooLong }},
		{name: "bucket", mutate: func(req *WorkspaceLeaseRequest) { req.Bucket = tooLong }},
		{name: "prefix", mutate: func(req *WorkspaceLeaseRequest) { req.Prefix = tooLong + "/" }},
		{name: "sandbox ID", mutate: func(req *WorkspaceLeaseRequest) { req.SandboxID = tooLong }},
		{name: "runtime", mutate: func(req *WorkspaceLeaseRequest) { req.Runtime = tooLong }},
		{name: "runtime ID", mutate: func(req *WorkspaceLeaseRequest) { req.RuntimeID = tooLong }},
		{name: "runtime UID", mutate: func(req *WorkspaceLeaseRequest) { req.RuntimeUID = tooLong }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newAtomicMemoryStore()
			c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
			req := validLeaseRequest()
			tt.mutate(&req)
			_, err := c.Acquire(context.Background(), req)
			require.ErrorIs(t, err, ErrInvalidWorkspaceLease)
			assert.NotContains(t, err.Error(), tooLong)
			assert.Zero(t, store.callCount())
		})
	}
}

func TestWorkspaceCoordinatorAcceptsMaximumPrefixBytes(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	req := validLeaseRequest()
	req.Prefix = strings.Repeat("a", workspaceLeaseMaxFieldBytes-1) + "/"
	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, c.Release(context.Background(), lease, runtime.TerminationEvidence{}))
}

func TestWorkspaceCoordinatorAcceptsEscapedMaximumFields(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	escaped := strings.Repeat("<", workspaceLeaseMaxFieldBytes)
	req := validLeaseRequest()
	req.Provider = escaped
	req.StorageIdentity = escaped
	req.Bucket = escaped
	req.SandboxID = escaped
	req.Runtime = escaped
	req.RuntimeID = escaped
	req.RuntimeUID = escaped

	lease, err := c.Acquire(context.Background(), req)
	require.NoError(t, err)
	require.NoError(t, c.Renew(context.Background(), lease))
}

func TestWorkspaceStateKeysAreSafeAndUnambiguous(t *testing.T) {
	a := validLeaseRequest()
	a.Provider, a.Bucket = "ab:c", "d"
	b := validLeaseRequest()
	b.Provider, b.Bucket = "ab", "c:d"
	keysA, err := workspaceStateKeys(a)
	require.NoError(t, err)
	keysB, err := workspaceStateKeys(b)
	require.NoError(t, err)
	assert.NotEqual(t, keysA.lease, keysB.lease)
	assert.NotContains(t, keysA.lease, a.StorageIdentity)
	assert.NotContains(t, keysA.lease, a.Prefix)
	assert.NotEqual(t, keysA.lease, keysA.owner)
	assert.NotEqual(t, keysA.owner, keysA.generation)
}

func TestWorkspaceCoordinatorStartRenewalIsImmediateAndCallsOnLostOnce(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, 120*time.Millisecond, 20*time.Millisecond)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	store.deleteKey(lease.Key)
	var calls atomic.Int32

	renewal, err := c.StartRenewal(context.Background(), lease, func(error) { calls.Add(1) })
	require.ErrorIs(t, err, ErrWorkspaceLeaseLost)
	assert.Nil(t, renewal)
	assert.Equal(t, int32(1), calls.Load())
}

func TestWorkspaceCoordinatorStartRenewalDetectsLaterLossOnce(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, 120*time.Millisecond, 20*time.Millisecond)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	var calls atomic.Int32
	renewal, err := c.StartRenewal(context.Background(), lease, func(error) { calls.Add(1) })
	require.NoError(t, err)

	store.deleteKey(lease.Key)
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	assert.Equal(t, int32(1), calls.Load())
	renewal.Stop()
}

func TestWorkspaceCoordinatorStoppingRenewalDoesNotReportLoss(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, 120*time.Millisecond, 20*time.Millisecond)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	var calls atomic.Int32
	renewal, err := c.StartRenewal(context.Background(), lease, func(error) { calls.Add(1) })
	require.NoError(t, err)
	renewal.Stop()
	store.deleteKey(lease.Key)
	time.Sleep(60 * time.Millisecond)
	assert.Zero(t, calls.Load())
}

func TestWorkspaceCoordinatorBindsWhileRenewalIsRunning(t *testing.T) {
	store := newAtomicMemoryStore()
	c := NewWorkspaceCoordinator(store, 120*time.Millisecond, 20*time.Millisecond)
	lease, err := c.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	renewal, err := c.StartRenewal(context.Background(), lease, nil)
	require.NoError(t, err)
	defer renewal.Stop()

	require.NoError(t, c.BindRuntime(context.Background(), lease, "uid-a"))
	auth, err := c.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	assert.Equal(t, "uid-a", auth.RuntimeUID)
}

func TestSessionStoreV2ListIgnoresWorkspaceStateNamespaces(t *testing.T) {
	store := newAtomicMemoryStore()
	sessions := NewSessionStore(store, time.Minute)
	sb := &Sandbox{ID: "session-a", CreatedAt: time.Now(), Timeout: time.Minute}
	require.NoError(t, sessions.Save(context.Background(), sb))
	require.NoError(t, store.Set(context.Background(), "sandbox:workspace:lease:anything", []byte("x"), 0))
	require.NoError(t, store.Set(context.Background(), "sandbox:workspace:owner:anything", []byte("x"), 0))
	require.NoError(t, store.Set(context.Background(), "sandbox:workspace:generation:anything", []byte("1"), 0))
	require.NoError(t, store.Set(context.Background(), "sandbox:legacy-session", []byte("{}"), 0))
	assert.True(t, store.hasKey("sandbox:session:v2:session-a"))

	ids, err := sessions.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"session-a"}, ids)
	loaded, err := sessions.Load(context.Background(), "session-a")
	require.NoError(t, err)
	assert.Equal(t, "session-a", loaded.ID)
}

func TestSessionStoreLoadMissingReturnsSandboxNotFound(t *testing.T) {
	store := newAtomicMemoryStore()
	sessions := NewSessionStore(store, time.Minute)

	_, err := sessions.Load(context.Background(), "missing")
	require.ErrorIs(t, err, ErrSandboxNotFound)
}

func TestSessionStoreLoadMigratesExactLegacySession(t *testing.T) {
	store := newAtomicMemoryStore()
	sessions := NewSessionStore(store, time.Minute)
	sb := &Sandbox{
		ID: "legacy-a", Config: SandboxConfig{Mode: ModePersistent}, State: StateReady,
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
		RuntimeID: "runtime-a", Timeout: time.Hour,
	}
	raw, err := json.Marshal(sb)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), "sandbox:"+sb.ID, raw, 0))

	loaded, err := sessions.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, loaded.ID)
	assert.True(t, store.hasKey(sandboxSessionKeyPrefix+sb.ID))
	assert.False(t, store.hasKey("sandbox:"+sb.ID))
}

func TestWorkspaceCoordinatorRestoresExactConsumedLeaseWithoutNewGeneration(t *testing.T) {
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := coordinator.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	require.NoError(t, coordinator.BindRuntime(context.Background(), lease, "uid-a"))
	_, err = coordinator.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	require.Equal(t, uint8(1), owner.MountAttempt)

	restored, err := coordinator.Restore(context.Background(), owner)
	require.NoError(t, err)
	assert.Equal(t, owner, restored.OwnerSnapshot())
	assert.Equal(t, lease.Key, restored.Key)
	require.NoError(t, coordinator.Renew(context.Background(), restored))
	store.mu.Lock()
	generation := store.increments[lease.generationKey]
	store.mu.Unlock()
	assert.Equal(t, int64(1), generation)
}

func TestWorkspaceCoordinatorRestoresExpiredLeaseFromExactOwner(t *testing.T) {
	store := newAtomicMemoryStore()
	coordinator := NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
	lease, err := coordinator.Acquire(context.Background(), validLeaseRequest())
	require.NoError(t, err)
	require.NoError(t, coordinator.BindRuntime(context.Background(), lease, "uid-a"))
	_, err = coordinator.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	store.expireKey(lease.Key)

	restored, err := coordinator.Restore(context.Background(), owner)
	require.NoError(t, err)
	assert.Equal(t, owner, restored.OwnerSnapshot())
	assert.NotEqual(t, string(lease.Value), string(restored.Value), "republication needs a fresh capability token")
	require.NoError(t, coordinator.Renew(context.Background(), restored))
	store.mu.Lock()
	generation := store.increments[lease.generationKey]
	store.mu.Unlock()
	assert.Equal(t, int64(1), generation)
}

func mustWorkspaceHash(t *testing.T, req WorkspaceLeaseRequest) string {
	t.Helper()
	keys, err := workspaceStateKeys(req)
	require.NoError(t, err)
	return keys.workspaceHash
}

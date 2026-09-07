package sandbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
)

type memoryFUSEPoolRepository struct {
	mu           sync.Mutex
	records      map[string]state.FUSEPoolRecord
	locks        map[string]memoryRefillLock
	now          time.Time
	stateHistory map[string][]state.FUSEPoolState
	failMethod   map[string]error
	failAfter    map[string]error
}

type memoryRefillLock struct {
	token string
	until time.Time
}

func newMemoryFUSEPoolRepository() *memoryFUSEPoolRepository {
	return &memoryFUSEPoolRepository{
		records:      make(map[string]state.FUSEPoolRecord),
		locks:        make(map[string]memoryRefillLock),
		now:          time.Now().UTC(),
		stateHistory: make(map[string][]state.FUSEPoolState),
		failMethod:   make(map[string]error),
		failAfter:    make(map[string]error),
	}
}

func (r *memoryFUSEPoolRepository) CreatePreparingWithAdmission(_ context.Context, record state.FUSEPoolRecord, refillToken string, maxSize int, prepareTTL time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("CreatePreparing"); err != nil {
		return err
	}
	if record.PreparationID == "" || record.RuntimeID != "" || record.RuntimeUID != "" || record.PoolKey == "" || record.MaintainerToken == "" || record.State != state.FUSEPoolPreparing || record.Revision != 1 || prepareTTL <= 0 {
		return state.ErrFUSEPoolInvalidRecord
	}
	lock, held := r.locks[record.PoolKey]
	if !held || lock.token != refillToken || !lock.until.After(r.now) {
		return state.ErrFUSEPoolTokenMismatch
	}
	if r.countPreparingAndPreparedLocked(record.PoolKey) >= maxSize {
		return state.ErrFUSEPoolConflict
	}
	if _, exists := r.records[record.PreparationID]; exists {
		return state.ErrFUSEPoolConflict
	}
	if record.ReservationToken != "" || !record.ReservedUntil.IsZero() {
		return state.ErrFUSEPoolInvalidRecord
	}
	record.UpdatedAt = r.now
	record.PrepareUntil = r.now.Add(prepareTTL)
	r.records[record.PreparationID] = record
	r.stateHistory[record.PreparationID] = append(r.stateHistory[record.PreparationID], record.State)
	return nil
}

func (r *memoryFUSEPoolRepository) BindPreparingRuntime(_ context.Context, preparationID, runtimeID, runtimeUID, refillToken string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("BindPreparingRuntime"); err != nil {
		return nil, err
	}
	record, ok := r.records[preparationID]
	if !ok {
		return nil, state.ErrFUSEPoolNotFound
	}
	lock := r.locks[record.PoolKey]
	if lock.token != refillToken || !lock.until.After(r.now) {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if record.State != state.FUSEPoolPreparing || record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.RuntimeID != "" || record.RuntimeUID != "" || runtimeID == "" || runtimeUID == "" {
		return nil, state.ErrFUSEPoolConflict
	}
	for id, existing := range r.records {
		if id != preparationID && existing.RuntimeUID == runtimeUID {
			return nil, state.ErrFUSEPoolConflict
		}
	}
	record.RuntimeID, record.RuntimeUID = runtimeID, runtimeUID
	record.UpdatedAt, record.Revision = r.now, record.Revision+1
	r.records[preparationID] = record
	copy := record
	return &copy, nil
}

func (r *memoryFUSEPoolRepository) ReservePrepared(_ context.Context, poolKey, token string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("ReservePrepared"); err != nil {
		return nil, err
	}
	if poolKey == "" || token == "" || ttl <= 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	type candidate struct{ id, uid string }
	candidates := make([]candidate, 0)
	for id, record := range r.records {
		if record.PoolKey == poolKey && record.State == state.FUSEPoolPrepared {
			candidates = append(candidates, candidate{id: id, uid: record.RuntimeUID})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].uid < candidates[j].uid })
	if len(candidates) == 0 {
		return nil, nil
	}
	id := candidates[0].id
	record := r.records[id]
	record.State = state.FUSEPoolReserved
	record.ReservationToken = token
	record.ReservedUntil = r.now.Add(ttl)
	record.UpdatedAt = r.now
	record.Revision++
	r.records[id] = record
	r.stateHistory[id] = append(r.stateHistory[id], record.State)
	copy := record
	return &copy, nil
}

func (r *memoryFUSEPoolRepository) Transition(_ context.Context, preparationID string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("Transition"); err != nil {
		return nil, err
	}
	return r.transitionLocked(preparationID, from, to, token, expectedRevision, 0)
}

func (r *memoryFUSEPoolRepository) transitionLocked(preparationID string, from, to state.FUSEPoolState, token string, expectedRevision uint64, reservationTTL time.Duration) (*state.FUSEPoolRecord, error) {
	record, ok := r.records[preparationID]
	if !ok {
		return nil, state.ErrFUSEPoolNotFound
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != from {
		return nil, state.ErrFUSEPoolConflict
	}
	switch {
	case from == state.FUSEPoolPreparing && to == state.FUSEPoolPrepared:
		if record.ReservationToken != "" {
			return nil, state.ErrFUSEPoolInvalidTransition
		}
		if token != record.MaintainerToken {
			return nil, state.ErrFUSEPoolTokenMismatch
		}
	case from == state.FUSEPoolPreparing && to == state.FUSEPoolReserved:
		if reservationTTL > 0 && record.ReservationToken == "" {
			record.ReservationToken = token
			record.ReservedUntil = r.now.Add(reservationTTL)
		}
		if record.ReservationToken == "" || token != record.ReservationToken || !record.ReservedUntil.After(r.now) {
			return nil, state.ErrFUSEPoolInvalidTransition
		}
	case from == state.FUSEPoolReserved && (to == state.FUSEPoolPrepared || to == state.FUSEPoolBinding):
		if token != record.ReservationToken || !record.ReservedUntil.After(r.now) {
			return nil, state.ErrFUSEPoolTokenMismatch
		}
		if to == state.FUSEPoolPrepared {
			record.ReservationToken = ""
			record.ReservedUntil = time.Time{}
		}
	case from == state.FUSEPoolBinding && to == state.FUSEPoolConsumed:
		if token != record.ReservationToken {
			return nil, state.ErrFUSEPoolTokenMismatch
		}
	default:
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	record.State = to
	record.UpdatedAt = r.now
	record.Revision++
	r.records[preparationID] = record
	r.stateHistory[preparationID] = append(r.stateHistory[preparationID], record.State)
	copy := record
	return &copy, nil
}

func (r *memoryFUSEPoolRepository) TransitionWithRefillLock(_ context.Context, preparationID string, from, to state.FUSEPoolState, token, refillToken string, expectedRevision uint64, reservationTTL time.Duration) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("TransitionWithRefillLock"); err != nil {
		return nil, err
	}
	record, ok := r.records[preparationID]
	if !ok {
		return nil, state.ErrFUSEPoolNotFound
	}
	lock := r.locks[record.PoolKey]
	if lock.token != refillToken || !lock.until.After(r.now) {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if (to == state.FUSEPoolReserved) != (reservationTTL > 0) {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	transitioned, err := r.transitionLocked(preparationID, from, to, token, expectedRevision, reservationTTL)
	if err != nil {
		return nil, err
	}
	if err := r.takeFailureLocked("TransitionWithRefillLockAfterCommit"); err != nil {
		return nil, err
	}
	return transitioned, nil
}

func (r *memoryFUSEPoolRepository) ReturnPreparedWithAdmission(_ context.Context, preparationID, reservationToken string, expectedRevision uint64, maxSize int) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("ReturnPreparedWithAdmission"); err != nil {
		return nil, err
	}
	record, ok := r.records[preparationID]
	if !ok {
		return nil, state.ErrFUSEPoolNotFound
	}
	if maxSize <= 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	if r.countPreparingAndPreparedLocked(record.PoolKey) >= maxSize {
		return nil, state.ErrFUSEPoolConflict
	}
	transitioned, err := r.transitionLocked(preparationID, state.FUSEPoolReserved, state.FUSEPoolPrepared, reservationToken, expectedRevision, 0)
	if err != nil {
		return nil, err
	}
	if err := r.takeFailureLocked("ReturnPreparedWithAdmissionAfterCommit"); err != nil {
		return nil, err
	}
	return transitioned, nil
}

func (r *memoryFUSEPoolRepository) ClaimCleanup(_ context.Context, preparationID string, from state.FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64, runtimeID, runtimeUID, cleanupToken string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[preparationID]
	if !ok {
		return nil, state.ErrFUSEPoolNotFound
	}
	sameToken := record.State == state.FUSEPoolCleanup && record.CleanupToken == cleanupToken
	if record.State == state.FUSEPoolCleanup {
		if !sameToken && record.CleanupUntil.After(r.now) {
			return nil, state.ErrFUSEPoolTokenMismatch
		}
	} else if record.State != from || record.Revision != expectedRevision || record.MaintainerToken != maintainerToken || record.ReservationToken != reservationToken {
		return nil, state.ErrFUSEPoolConflict
	}
	evidenceAdded := false
	if record.RuntimeID == "" {
		if (runtimeID == "") != (runtimeUID == "") {
			return nil, state.ErrFUSEPoolInvalidRecord
		}
		for id, existing := range r.records {
			if runtimeUID != "" && id != preparationID && existing.RuntimeUID == runtimeUID {
				return nil, state.ErrFUSEPoolConflict
			}
		}
		record.RuntimeID, record.RuntimeUID = runtimeID, runtimeUID
		evidenceAdded = runtimeID != ""
	} else if record.RuntimeID != runtimeID || record.RuntimeUID != runtimeUID {
		return nil, state.ErrFUSEPoolConflict
	}
	if sameToken && !evidenceAdded {
		copy := record
		return &copy, nil
	}
	record.State, record.CleanupToken = state.FUSEPoolCleanup, cleanupToken
	if !sameToken {
		record.CleanupUntil = r.now.Add(ttl)
	}
	record.UpdatedAt, record.Revision = r.now, record.Revision+1
	r.records[preparationID] = record
	r.stateHistory[preparationID] = append(r.stateHistory[preparationID], record.State)
	copy := record
	return &copy, nil
}

func (r *memoryFUSEPoolRepository) ListByPoolKey(_ context.Context, poolKey string) ([]state.FUSEPoolRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("ListByPoolKey"); err != nil {
		return nil, err
	}
	result := make([]state.FUSEPoolRecord, 0)
	for _, record := range r.records {
		if record.PoolKey == poolKey {
			result = append(result, record)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RuntimeUID < result[j].RuntimeUID })
	return result, nil
}

func (r *memoryFUSEPoolRepository) ListPoolKeys(_ context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0)
	seen := make(map[string]struct{})
	for _, record := range r.records {
		if _, exists := seen[record.PoolKey]; exists {
			continue
		}
		seen[record.PoolKey] = struct{}{}
		keys = append(keys, record.PoolKey)
	}
	sort.Strings(keys)
	return keys, nil
}

func (r *memoryFUSEPoolRepository) DrainRefillLocks(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.locks = make(map[string]memoryRefillLock)
	return nil
}

func (r *memoryFUSEPoolRepository) CountPreparingAndPrepared(_ context.Context, poolKey string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("CountPreparingAndPrepared"); err != nil {
		return 0, err
	}
	return r.countPreparingAndPreparedLocked(poolKey), nil
}

func (r *memoryFUSEPoolRepository) DeleteCleanup(_ context.Context, preparationID, cleanupToken string, expectedRevision uint64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("DeleteCleanup"); err != nil {
		return false, err
	}
	record, ok := r.records[preparationID]
	if !ok {
		return false, state.ErrFUSEPoolNotFound
	}
	if record.State != state.FUSEPoolCleanup || record.CleanupToken != cleanupToken {
		return false, state.ErrFUSEPoolTokenMismatch
	}
	if record.Revision != expectedRevision {
		return false, state.ErrFUSEPoolCASMismatch
	}
	delete(r.records, preparationID)
	return true, nil
}

func (r *memoryFUSEPoolRepository) ServerTime(_ context.Context) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("ServerTime"); err != nil {
		return time.Time{}, err
	}
	return r.now, nil
}

func (r *memoryFUSEPoolRepository) TryRefillLock(_ context.Context, poolKey, token string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("TryRefillLock"); err != nil {
		return false, err
	}
	if poolKey == "" || token == "" || ttl <= 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	if lock, held := r.locks[poolKey]; held && lock.until.After(r.now) {
		return false, nil
	}
	r.locks[poolKey] = memoryRefillLock{token: token, until: r.now.Add(ttl)}
	return true, nil
}

func (r *memoryFUSEPoolRepository) RenewRefillLock(_ context.Context, poolKey, token string, ttl time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("RenewRefillLock"); err != nil {
		return false, err
	}
	lock, ok := r.locks[poolKey]
	if !ok || lock.token != token || !lock.until.After(r.now) {
		return false, nil
	}
	r.locks[poolKey] = memoryRefillLock{token: token, until: r.now.Add(ttl)}
	return true, nil
}

func (r *memoryFUSEPoolRepository) UnlockRefill(_ context.Context, poolKey, token string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.takeFailureLocked("UnlockRefill"); err != nil {
		return err
	}
	if r.locks[poolKey].token != token {
		return state.ErrFUSEPoolTokenMismatch
	}
	delete(r.locks, poolKey)
	return nil
}

func (r *memoryFUSEPoolRepository) seed(records ...state.FUSEPoolRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range records {
		if record.PreparationID == "" {
			record.PreparationID = record.RuntimeUID
		}
		if record.PrepareUntil.IsZero() {
			record.PrepareUntil = r.now.Add(time.Minute)
		}
		r.records[record.PreparationID] = record
		r.stateHistory[record.PreparationID] = append(r.stateHistory[record.PreparationID], record.State)
	}
}

func (r *memoryFUSEPoolRepository) record(preparationID string) (state.FUSEPoolRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[preparationID]
	return record, ok
}

func (r *memoryFUSEPoolRepository) recordByRuntimeUID(runtimeUID string) (state.FUSEPoolRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.RuntimeUID == runtimeUID {
			return record, true
		}
	}
	return state.FUSEPoolRecord{}, false
}

func (r *memoryFUSEPoolRepository) countState(poolKey string, wanted state.FUSEPoolState) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, record := range r.records {
		if record.PoolKey == poolKey && record.State == wanted {
			count++
		}
	}
	return count
}

func (r *memoryFUSEPoolRepository) countPreparingAndPrepared(poolKey string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.countPreparingAndPreparedLocked(poolKey)
}

func (r *memoryFUSEPoolRepository) countPreparingAndPreparedLocked(poolKey string) int {
	count := 0
	for _, record := range r.records {
		if record.PoolKey == poolKey && (record.State == state.FUSEPoolPreparing || record.State == state.FUSEPoolPrepared) {
			count++
		}
	}
	return count
}

func (r *memoryFUSEPoolRepository) history(preparationID string) []state.FUSEPoolState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]state.FUSEPoolState(nil), r.stateHistory[preparationID]...)
}

func (r *memoryFUSEPoolRepository) failNext(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failMethod[method] = err
}

func (r *memoryFUSEPoolRepository) failNextAfterCommit(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failAfter[method+"AfterCommit"] = err
}

func (r *memoryFUSEPoolRepository) takeFailureLocked(method string) error {
	err := r.failMethod[method]
	delete(r.failMethod, method)
	if err == nil {
		err = r.failAfter[method]
		delete(r.failAfter, method)
	}
	return err
}

var _ state.FUSEPoolRepository = (*memoryFUSEPoolRepository)(nil)

type advancingAfterReturnRepository struct {
	*memoryFUSEPoolRepository
	err      error
	advanced *state.FUSEPoolRecord
}

func (r *advancingAfterReturnRepository) ReturnPreparedWithAdmission(ctx context.Context, preparationID, reservationToken string, expectedRevision uint64, maxSize int) (*state.FUSEPoolRecord, error) {
	returned, err := r.memoryFUSEPoolRepository.ReturnPreparedWithAdmission(ctx, preparationID, reservationToken, expectedRevision, maxSize)
	if err != nil {
		return nil, err
	}
	advanced, reserveErr := r.memoryFUSEPoolRepository.ReservePrepared(ctx, returned.PoolKey, "next-owner", time.Minute)
	if reserveErr != nil {
		return nil, reserveErr
	}
	r.advanced = advanced
	return nil, r.err
}

type blockingAfterReturnRepository struct {
	*memoryFUSEPoolRepository
	entered chan struct{}
	release chan struct{}
}

func (r *blockingAfterReturnRepository) ReturnPreparedWithAdmission(ctx context.Context, preparationID, reservationToken string, expectedRevision uint64, maxSize int) (*state.FUSEPoolRecord, error) {
	returned, err := r.memoryFUSEPoolRepository.ReturnPreparedWithAdmission(ctx, preparationID, reservationToken, expectedRevision, maxSize)
	if err != nil {
		return nil, err
	}
	close(r.entered)
	<-r.release
	return returned, nil
}

type blockingAfterTransitionRepository struct {
	*memoryFUSEPoolRepository
	entered chan struct{}
	release chan struct{}
}

func (r *blockingAfterTransitionRepository) TransitionWithRefillLock(ctx context.Context, preparationID string, from, to state.FUSEPoolState, token, refillToken string, expectedRevision uint64, reservationTTL time.Duration) (*state.FUSEPoolRecord, error) {
	transitioned, err := r.memoryFUSEPoolRepository.TransitionWithRefillLock(ctx, preparationID, from, to, token, refillToken, expectedRevision, reservationTTL)
	if err != nil {
		return nil, err
	}
	close(r.entered)
	<-r.release
	return transitioned, nil
}

func fixedFUSESpec(poolKey string) runtime.SandboxSpec {
	return runtime.SandboxSpec{
		ID: "sandbox-pool-template", Image: "sandbox:latest", Memory: "512Mi", MemoryRequest: "128Mi",
		CPU: "500m", CPURequest: "100m", Disk: "1Gi", TmpDisk: "64Mi", PidLimit: 100,
		ReadOnlyRootFS: true, RunAsUser: 1000, SeccompProfile: "RuntimeDefault",
		WorkspaceFUSE: &runtime.WorkspaceFUSESpec{
			RuntimeType: "kubernetes", Provider: "minio", Driver: "s3fs", Profile: "minio-primary",
			StorageIdentity: "storage-primary", CredentialGeneration: "generation-1",
			MounterImage: "mounter@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SecretName:   "storage-secret", CASecretKey: "ca.crt", EndpointHostIPs: []string{"192.0.2.11", "192.0.2.10"},
			Bucket: "sandbox", Endpoint: "minio.example.com:9000", Region: "us-east-1", UseSSL: true,
			CacheSize: "2Gi", CacheMedium: "disk", MountTimeout: 30 * time.Second, FlushTimeout: 30 * time.Second, UnmountTimeout: 15 * time.Second,
			LSMProfile: "sandbox-fuse", PoolKey: poolKey,
			MounterResources: runtime.WorkspaceFUSEResources{
				CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "512Mi",
				EphemeralStorageRequest: "2Gi", EphemeralStorageLimit: "4Gi",
			},
			SystemEgress: runtime.SystemEgressSpec{
				Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32", "8.8.8.8/32"}, DNSPorts: []int32{53},
				EndpointCIDRs: []string{"192.0.2.0/24"}, EndpointFQDNs: []string{"minio.example.com"}, EndpointPorts: []int32{9000},
			},
		},
	}
}

func newFUSEMockRuntime() *mockRuntime { return newMockRuntime() }

func allowPreparedReturn(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
	return FUSEPoolPristine, nil
}

func fusePoolConfig() FUSEPoolConfig {
	return FUSEPoolConfig{
		MinSize: 1, MaxSize: 3, RefillInterval: 10 * time.Millisecond,
		PrepareTimeout: time.Second, ReservationTTL: time.Minute,
		MaintainerToken: "api-a", PristineGuard: allowPreparedReturn,
	}
}

func TestComputeFUSEPoolKeyCanonicalProjection(t *testing.T) {
	a := fixedFUSESpec("ignored-a")
	b := fixedFUSESpec("ignored-b")
	b.ID = "request-id"
	b.Labels = map[string]string{"sandbox.id": "request-id"}
	b.NetworkEnabled = true
	b.NetworkWhitelist = []string{"user.example.com"}
	b.NetworkBlockPrivate = true
	b.WorkspaceFUSE.EndpointHostIPs = []string{"192.0.2.10", "192.0.2.11", "192.0.2.10"}
	b.WorkspaceFUSE.SystemEgress.DNSCIDRs = []string{"8.8.8.8/32", "1.1.1.1/32", "8.8.8.8/32"}
	b.WorkspaceFUSE.SystemEgress.DNSPorts = []int32{53, 53}
	b.WorkspaceFUSE.SystemEgress.EndpointCIDRs = []string{"192.0.2.0/24", "192.0.2.0/24"}
	b.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"minio.example.com", "minio.example.com"}
	b.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{9000, 9000}

	keyA, err := ComputeFUSEPoolKey(a)
	require.NoError(t, err)
	keyB, err := ComputeFUSEPoolKey(b)
	require.NoError(t, err)
	assert.Equal(t, keyA, keyB, "request fields and set ordering must not affect the pool key")

	mutations := []struct {
		name string
		edit func(*runtime.SandboxSpec)
	}{
		{"runtime type", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.RuntimeType = "docker" }},
		{"image", func(s *runtime.SandboxSpec) { s.Image = "sandbox:v2" }},
		{"memory", func(s *runtime.SandboxSpec) { s.Memory = "1Gi" }},
		{"memory request", func(s *runtime.SandboxSpec) { s.MemoryRequest = "256Mi" }},
		{"cpu", func(s *runtime.SandboxSpec) { s.CPU = "1" }},
		{"cpu request", func(s *runtime.SandboxSpec) { s.CPURequest = "200m" }},
		{"disk", func(s *runtime.SandboxSpec) { s.Disk = "2Gi" }},
		{"tmp disk", func(s *runtime.SandboxSpec) { s.TmpDisk = "128Mi" }},
		{"pid limit", func(s *runtime.SandboxSpec) { s.PidLimit = 101 }},
		{"read only root", func(s *runtime.SandboxSpec) { s.ReadOnlyRootFS = false }},
		{"run as user", func(s *runtime.SandboxSpec) { s.RunAsUser = 1001 }},
		{"security", func(s *runtime.SandboxSpec) { s.SeccompProfile = "Localhost/fuse" }},
		{"provider", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Provider = "obs" }},
		{"driver", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Driver = "other" }},
		{"profile", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Profile = "obs-private" }},
		{"storage identity", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.StorageIdentity = "storage-secondary" }},
		{"credential generation", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CredentialGeneration = "generation-2" }},
		{"mounter image", func(s *runtime.SandboxSpec) {
			s.WorkspaceFUSE.MounterImage = "mounter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		}},
		{"docker image", func(s *runtime.SandboxSpec) {
			s.WorkspaceFUSE.DockerImage = "sandbox-fuse@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{"secret", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SecretName = "storage-secret-v2" }},
		{"CA", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CASecretKey = "private-ca.crt" }},
		{"endpoint host IP", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.EndpointHostIPs = []string{"192.0.2.12"} }},
		{"bucket", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Bucket = "sandbox-v2" }},
		{"endpoint", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Endpoint = "minio-v2.example.com:9000" }},
		{"region", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.Region = "cn-north-4" }},
		{"TLS", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.UseSSL = false }},
		{"cache size", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CacheSize = "3Gi" }},
		{"cache medium", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.CacheMedium = "memory" }},
		{"mount timeout", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MountTimeout = time.Minute }},
		{"flush timeout", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.FlushTimeout = time.Minute }},
		{"unmount timeout", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.UnmountTimeout = time.Minute }},
		{"LSM", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.LSMProfile = "sandbox-fuse-v2" }},
		{"mounter CPU request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.CPURequest = "200m" }},
		{"mounter CPU", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.CPULimit = "1" }},
		{"mounter memory request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.MemoryRequest = "256Mi" }},
		{"mounter memory", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.MemoryLimit = "1Gi" }},
		{"mounter ephemeral request", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.EphemeralStorageRequest = "3Gi" }},
		{"mounter ephemeral", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.MounterResources.EphemeralStorageLimit = "8Gi" }},
		{"egress mode", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.Mode = runtime.SystemEgressCiliumFQDN }},
		{"DNS CIDR", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.DNSCIDRs = []string{"9.9.9.9/32"} }},
		{"DNS port", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.DNSPorts = []int32{5353} }},
		{"endpoint CIDR", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.EndpointCIDRs = []string{"198.51.100.0/24"} }},
		{"endpoint FQDN", func(s *runtime.SandboxSpec) {
			s.WorkspaceFUSE.SystemEgress.EndpointFQDNs = []string{"objects.example.com"}
		}},
		{"endpoint port", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.EndpointPorts = []int32{443} }},
		{"proxy URL", func(s *runtime.SandboxSpec) { s.WorkspaceFUSE.SystemEgress.ProxyURL = "http://proxy.example.com" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := fixedFUSESpec("anything")
			mutation.edit(&changed)
			key, gotErr := ComputeFUSEPoolKey(changed)
			require.NoError(t, gotErr)
			assert.NotEqual(t, keyA, key)
		})
	}

	assert.Equal(t, []string{"192.0.2.11", "192.0.2.10"}, a.WorkspaceFUSE.EndpointHostIPs, "hashing must not mutate caller slices")
}

func TestFUSEPoolWarmAcquireAndRefill(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 2, 3
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	require.NoError(t, pool.WarmUp(context.Background()))
	require.Equal(t, 2, repo.countState("pool-key", state.FUSEPoolPrepared))
	got, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	assert.Equal(t, state.FUSEPoolReserved, got.State)
	require.Eventually(t, func() bool { return repo.countState("pool-key", state.FUSEPoolPrepared) == 2 }, time.Second, 5*time.Millisecond)
	assert.LessOrEqual(t, repo.countPreparingAndPrepared("pool-key"), 3)
	pool.Stop(context.Background())
}

func TestFUSEPoolUsesSyncCompatibleConciseRuntimeName(t *testing.T) {
	rt := newFUSEMockRuntime()
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 0, 1
	pool := NewFUSEPool(rt, newMemoryFUSEPoolRepository(), cfg, fixedFUSESpec("pool-key"))

	record, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	assert.Regexp(t, `^sandbox-pool-[a-z0-9]{10}$`, rt.lastCreatedSpec().ID)
	assert.Equal(t, rt.lastCreatedSpec().ID, record.PreparationID)
	pool.Stop(context.Background())
}

func TestFUSEPoolColdAcquireNeverPublishesPrepared(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	record, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, record.State)
	assert.Equal(t, []state.FUSEPoolState{state.FUSEPoolPreparing, state.FUSEPoolReserved}, repo.history(record.PreparationID))
	assert.NotEmpty(t, record.ReservationToken)
}

func TestFUSEPoolAcquireWaitsForConcurrentRefill(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize, cfg.PrepareTimeout = 0, 1, 250*time.Millisecond
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	record := state.FUSEPoolRecord{
		PreparationID:   "prep-waiting",
		RuntimeID:       "runtime-waiting",
		RuntimeUID:      "uid-waiting",
		PoolKey:         "pool-key",
		State:           state.FUSEPoolPrepared,
		MaintainerToken: "other-api",
		UpdatedAt:       repo.now,
		Revision:        2,
	}
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	repo.locks[record.PoolKey] = memoryRefillLock{token: "other-refill", until: repo.now.Add(time.Minute)}

	published := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		repo.seed(record)
		close(published)
	}()

	got, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, record.RuntimeUID, got.RuntimeUID)
	assert.Equal(t, state.FUSEPoolReserved, got.State)
	<-published
	pool.Stop(context.Background())
}

func TestFUSEPoolAcquireRefillWaitHonorsCallerCancellation(t *testing.T) {
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize, cfg.PrepareTimeout = 0, 1, time.Second
	pool := NewFUSEPool(newFUSEMockRuntime(), repo, cfg, fixedFUSESpec("pool-key"))
	repo.locks["pool-key"] = memoryRefillLock{token: "other-refill", until: repo.now.Add(time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := pool.Acquire(ctx, "pool-key")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	pool.Stop(context.Background())
}

func TestFUSEPoolColdAcquireHonorsMaxAcrossReplicas(t *testing.T) {
	repo := newMemoryFUSEPoolRepository()
	rtA, rtB := newFUSEMockRuntime(), newFUSEMockRuntime()
	cfgA := fusePoolConfig()
	cfgA.MinSize, cfgA.MaxSize = 0, 1
	cfgB := cfgA
	cfgB.MaintainerToken = "api-b"
	cfgB.PrepareTimeout = 20 * time.Millisecond
	poolA := NewFUSEPool(rtA, repo, cfgA, fixedFUSESpec("pool-key"))
	poolB := NewFUSEPool(rtB, repo, cfgB, fixedFUSESpec("pool-key"))
	entered, release := rtA.blockPrepare()
	type result struct {
		record *state.FUSEPoolRecord
		err    error
	}
	resultA := make(chan result, 1)
	go func() {
		record, err := poolA.Acquire(context.Background(), "pool-key")
		resultA <- result{record: record, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first cold acquire did not enter preparation")
	}
	assert.Equal(t, 1, repo.countState("pool-key", state.FUSEPoolPreparing), "capacity intent must exist before runtime creation completes")

	_, err := poolB.Acquire(context.Background(), "pool-key")
	require.ErrorIs(t, err, ErrFUSEPoolRefillBusy)
	assert.LessOrEqual(t, repo.countPreparingAndPrepared("pool-key"), 1)
	close(release)
	select {
	case got := <-resultA:
		require.NoError(t, got.err)
		require.NotNil(t, got.record)
	case <-time.After(time.Second):
		t.Fatal("first cold acquire did not finish")
	}
}

func TestFUSEPoolCleanupClaimPreventsOldPreparedSnapshotDeletingReservedRuntime(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: repo.now, Revision: 2}
	repo.seed(record)
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	snapshot, ok := repo.record(record.RuntimeUID)
	require.True(t, ok)
	reserved, err := repo.ReservePrepared(context.Background(), "pool-key", "reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)

	err = pool.claimAndDestroy(context.Background(), snapshot)
	require.Error(t, err)
	assert.False(t, rt.wasRemoved(record.RuntimeID), "runtime removal must happen only after an exact cleanup claim")
	current, ok := repo.record(record.RuntimeUID)
	require.True(t, ok)
	assert.Equal(t, state.FUSEPoolReserved, current.State)
}

func TestFUSEPoolAcquireProtectedOrUnknownNeverDeletesRuntime(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition FUSEPoolDisposition
		err         error
	}{
		{name: "protected", disposition: FUSEPoolProtected},
		{name: "unknown", disposition: FUSEPoolDispositionUnknown, err: errors.New("owner store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFUSEMockRuntime()
			repo := newMemoryFUSEPoolRepository()
			cfg := fusePoolConfig()
			cfg.MinSize = 0
			pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
			record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: repo.now, Revision: 2}
			repo.seed(record)
			rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
			pool.config.PristineGuard = func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
				return tc.disposition, tc.err
			}

			got, err := pool.Acquire(context.Background(), "pool-key")
			assert.Nil(t, got)
			require.Error(t, err)
			assert.False(t, rt.wasRemoved(record.RuntimeID))
			retained, ok := repo.record(record.RuntimeUID)
			require.True(t, ok)
			assert.Equal(t, state.FUSEPoolReserved, retained.State)
		})
	}
}

func TestFUSEPoolReconcileAtomicallyQuarantinesUnprovenPrepared(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: repo.now, Revision: 2}
	repo.seed(record)
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	cfg.PristineGuard = func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
		return FUSEPoolProtected, nil
	}
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	require.ErrorIs(t, pool.Reconcile(context.Background()), ErrFUSEPoolProtected)
	retained, ok := repo.record(record.RuntimeUID)
	require.True(t, ok)
	assert.Equal(t, state.FUSEPoolReserved, retained.State, "unproven record must leave the claimable prepared set atomically")
	assert.Equal(t, 0, repo.countPreparingAndPrepared("pool-key"))
	assert.False(t, rt.wasRemoved(record.RuntimeID))
}

func TestFUSEPoolReconcileUsesRepositoryServerTime(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	repo.now = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-a", UpdatedAt: time.Unix(1, 0), PrepareUntil: repo.now.Add(time.Minute), Revision: 1}
	repo.seed(record)
	poolCfg := fusePoolConfig()
	poolCfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, poolCfg, fixedFUSESpec("pool-key"))

	require.NoError(t, pool.Reconcile(context.Background()))
	_, ok := repo.record(record.RuntimeUID)
	assert.True(t, ok, "host UpdatedAt/clock must not override Redis PrepareUntil")
}

func TestFUSEPoolLostRefillLockCancelsPreparationAndLeavesNoReservation(t *testing.T) {
	rt := newFUSEMockRuntime()
	entered, _ := rt.blockPrepare()
	repo := newMemoryFUSEPoolRepository()
	repo.failNext("RenewRefillLock", errors.New("lost lock"))
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize, cfg.PrepareTimeout = 0, 1, 60*time.Millisecond
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	result := make(chan error, 1)
	go func() { _, err := pool.Acquire(context.Background(), "pool-key"); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prepare did not start")
	}
	select {
	case err := <-result:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("lost lock did not cancel preparation")
	}
	assert.Equal(t, 0, repo.countState("pool-key", state.FUSEPoolReserved))
}

func TestFUSEPoolExpiredLockAllowsSecondReplicaAndFencesFirst(t *testing.T) {
	repo := newMemoryFUSEPoolRepository()
	rtA, rtB := newFUSEMockRuntime(), newFUSEMockRuntime()
	entered, _ := rtA.blockPrepare()
	cfgA := fusePoolConfig()
	cfgA.MinSize, cfgA.MaxSize, cfgA.PrepareTimeout = 0, 1, 300*time.Millisecond
	cfgB := cfgA
	cfgB.MinSize = 1
	cfgB.MaintainerToken = "api-b"
	poolA := NewFUSEPool(rtA, repo, cfgA, fixedFUSESpec("pool-key"))
	poolB := NewFUSEPool(rtB, repo, cfgB, fixedFUSESpec("pool-key"))
	first := make(chan error, 1)
	go func() { _, err := poolA.Acquire(context.Background(), "pool-key"); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first replica did not start preparation")
	}
	repo.mu.Lock()
	repo.now = repo.now.Add(time.Second)
	repo.mu.Unlock()
	require.NoError(t, poolB.WarmUp(context.Background()))
	select {
	case err := <-first:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("expired-lock controller was not fenced")
	}
	assert.Equal(t, 1, repo.countState("pool-key", state.FUSEPoolPrepared))
	assert.Equal(t, 0, repo.countState("pool-key", state.FUSEPoolReserved))
}

func TestFUSEPoolBindFailurePreservesRuntimeEvidenceWhenCleanupFails(t *testing.T) {
	rt := newFUSEMockRuntime()
	rt.failRemove("prepared-runtime-1", errors.New("runtime unavailable"))
	repo := newMemoryFUSEPoolRepository()
	repo.failNext("BindPreparingRuntime", state.ErrFUSEPoolTokenMismatch)
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	_, err := pool.Acquire(context.Background(), "pool-key")
	require.Error(t, err)
	records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, listErr)
	require.Len(t, records, 1)
	assert.Equal(t, state.FUSEPoolCleanup, records[0].State)
	assert.Equal(t, "prepared-runtime-1", records[0].RuntimeID)
	assert.Equal(t, "prepared-uid-1", records[0].RuntimeUID)
}

func TestFUSEPoolAcquireDiscardsFailedPristineProbe(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize = 2
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	require.NoError(t, pool.WarmUp(context.Background()))
	first, ok := repo.recordByRuntimeUID("prepared-uid-1")
	require.True(t, ok)
	rt.failPreparedHealth(first.RuntimeID, errors.New("not pristine"))

	got, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	assert.Equal(t, "prepared-uid-2", got.RuntimeUID)
	assert.True(t, rt.wasRemoved(first.RuntimeID))
	_, exists := repo.record(first.PreparationID)
	assert.False(t, exists)
}

func TestFUSEPoolReturnPreparedRequiresPristineAndNoOwnerReferences(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
		require.NoError(t, pool.WarmUp(context.Background()))
		record, err := pool.Acquire(context.Background(), "pool-key")
		require.NoError(t, err)
		require.NoError(t, pool.ReturnPrepared(context.Background(), *record))
		require.Eventually(t, func() bool {
			returned, ok := repo.record(record.PreparationID)
			return ok && returned.State == state.FUSEPoolPrepared
		}, time.Second, time.Millisecond)
	})

	t.Run("guard missing fails closed before pool operation", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		cfg := fusePoolConfig()
		cfg.PristineGuard = nil
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
		require.ErrorIs(t, pool.WarmUp(context.Background()), ErrInvalidFUSEPoolConfig)
		assert.Equal(t, 0, rt.prepareCount())
	})
}

func TestFUSEPoolReturnPreparedCannotPublishAfterStopStarts(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfg.PristineGuard = func(ctx context.Context, record state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
		if record.State != state.FUSEPoolReserved {
			return FUSEPoolPristine, nil
		}
		once.Do(func() { close(entered) })
		<-release // deliberately ignores cancellation to prove Stop waits for opWG
		return FUSEPoolPristine, nil
	}
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolReserved, MaintainerToken: "api-a", ReservationToken: "reservation", ReservedUntil: repo.now.Add(time.Minute), UpdatedAt: repo.now, Revision: 3}
	repo.seed(record)
	returned := make(chan error, 1)
	go func() {
		returned <- pool.ReturnPrepared(context.Background(), func() state.FUSEPoolRecord { got, _ := repo.record(record.RuntimeUID); return got }())
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("return guard did not block")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- pool.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop completed before in-flight return drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-returned:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("ReturnPrepared did not finish")
	}
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish")
	}
	retained, ok := repo.record(record.RuntimeUID)
	require.True(t, ok)
	assert.NotEqual(t, state.FUSEPoolPrepared, retained.State)
}

func TestFUSEPoolReturnAtCapacityDestroysReservation(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 1, 1
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	require.NoError(t, pool.WarmUp(context.Background()))
	old, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return repo.countState("pool-key", state.FUSEPoolPrepared) == 1 }, time.Second, 5*time.Millisecond)
	err = pool.ReturnPrepared(context.Background(), *old)
	require.Error(t, err)
	_, exists := repo.record(old.PreparationID)
	assert.False(t, exists)
	assert.True(t, rt.wasRemoved(old.RuntimeID))
	assert.LessOrEqual(t, repo.countPreparingAndPrepared("pool-key"), 1)
}

func TestFUSEPoolCompensatesFinalPublicationOutcomes(t *testing.T) {
	networkErr := errors.New("redis reply lost after commit")
	newReserved := func(repo *memoryFUSEPoolRepository) state.FUSEPoolRecord {
		return state.FUSEPoolRecord{
			PreparationID: "preparation-a", RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key",
			State: state.FUSEPoolReserved, MaintainerToken: "api-a", ReservationToken: "reservation-a",
			ReservedUntil: repo.now.Add(time.Minute), PrepareUntil: repo.now.Add(time.Minute), UpdatedAt: repo.now, Revision: 3,
		}
	}

	t.Run("return", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		record := newReserved(repo)
		repo.seed(record)
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
		repo.failNextAfterCommit("ReturnPreparedWithAdmission", networkErr)
		cfg := fusePoolConfig()
		cfg.MinSize = 0
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		err := pool.ReturnPrepared(context.Background(), record)
		require.ErrorIs(t, err, networkErr)
		_, exists := repo.record(record.PreparationID)
		assert.False(t, exists)
		assert.True(t, rt.wasRemoved(record.RuntimeID))
	})

	t.Run("inspection", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		record := newReserved(repo)
		record.State, record.ReservationToken, record.ReservedUntil, record.Revision = state.FUSEPoolPrepared, "", time.Time{}, 2
		repo.seed(record)
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
		repo.failNextAfterCommit("ReturnPreparedWithAdmission", networkErr)
		cfg := fusePoolConfig()
		cfg.MinSize = 0
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		err := pool.Reconcile(context.Background())
		require.ErrorIs(t, err, networkErr)
		_, exists := repo.record(record.PreparationID)
		assert.False(t, exists)
		assert.True(t, rt.wasRemoved(record.RuntimeID))
	})

	t.Run("warm publish", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		repo.failNextAfterCommit("TransitionWithRefillLock", networkErr)
		cfg := fusePoolConfig()
		cfg.MinSize, cfg.MaxSize = 1, 1
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		err := pool.WarmUp(context.Background())
		require.ErrorIs(t, err, networkErr)
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		require.NoError(t, listErr)
		assert.Empty(t, records)
		assert.True(t, rt.wasRemoved("prepared-runtime-1"))
	})

	t.Run("cold publish", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		repo.failNextAfterCommit("TransitionWithRefillLock", networkErr)
		cfg := fusePoolConfig()
		cfg.MinSize, cfg.MaxSize = 0, 1
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		record, err := pool.Acquire(context.Background(), "pool-key")
		assert.Nil(t, record)
		require.ErrorIs(t, err, networkErr)
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		require.NoError(t, listErr)
		assert.Empty(t, records)
		assert.True(t, rt.wasRemoved("prepared-runtime-1"))
	})

	t.Run("warm publish not committed", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		repo.failNext("TransitionWithRefillLock", networkErr)
		cfg := fusePoolConfig()
		cfg.MinSize, cfg.MaxSize = 1, 1
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		err := pool.WarmUp(context.Background())
		require.ErrorIs(t, err, networkErr)
		records, listErr := repo.ListByPoolKey(context.Background(), "pool-key")
		require.NoError(t, listErr)
		assert.Empty(t, records, "exact before-state compensation must clean an uncommitted publication")
		assert.True(t, rt.wasRemoved("prepared-runtime-1"))
	})
}

func TestFUSEPoolPublicationCompensationNeverDeletesAdvancedAcquire(t *testing.T) {
	rt := newFUSEMockRuntime()
	base := newMemoryFUSEPoolRepository()
	record := state.FUSEPoolRecord{
		PreparationID: "preparation-a", RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key",
		State: state.FUSEPoolReserved, MaintainerToken: "api-a", ReservationToken: "reservation-a",
		ReservedUntil: base.now.Add(time.Minute), PrepareUntil: base.now.Add(time.Minute), UpdatedAt: base.now, Revision: 3,
	}
	base.seed(record)
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	replyErr := errors.New("reply lost after next owner acquired")
	repo := &advancingAfterReturnRepository{memoryFUSEPoolRepository: base, err: replyErr}
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	err := pool.ReturnPrepared(context.Background(), record)
	require.ErrorIs(t, err, replyErr)
	require.NotNil(t, repo.advanced)
	current, exists := base.record(record.PreparationID)
	require.True(t, exists)
	assert.Equal(t, state.FUSEPoolReserved, current.State)
	assert.Equal(t, "next-owner", current.ReservationToken)
	assert.False(t, rt.wasRemoved(record.RuntimeID), "failed exact compensation must not delete the next owner's runtime")
}

func TestFUSEPoolStopAfterReturnCommitCompensatesPublishedRecord(t *testing.T) {
	rt := newFUSEMockRuntime()
	base := newMemoryFUSEPoolRepository()
	record := state.FUSEPoolRecord{
		PreparationID: "preparation-a", RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key",
		State: state.FUSEPoolReserved, MaintainerToken: "api-a", ReservationToken: "reservation-a",
		ReservedUntil: base.now.Add(time.Minute), PrepareUntil: base.now.Add(time.Minute), UpdatedAt: base.now, Revision: 3,
	}
	base.seed(record)
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	repo := &blockingAfterReturnRepository{memoryFUSEPoolRepository: base, entered: make(chan struct{}), release: make(chan struct{})}
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	returned := make(chan error, 1)
	go func() { returned <- pool.ReturnPrepared(context.Background(), record) }()
	<-repo.entered
	stopped := make(chan error, 1)
	go func() { stopped <- pool.Stop(context.Background()) }()
	require.Eventually(t, func() bool {
		pool.lifecycleMu.Lock()
		defer pool.lifecycleMu.Unlock()
		return pool.stopping
	}, time.Second, time.Millisecond)
	close(repo.release)
	require.ErrorIs(t, <-returned, ErrFUSEPoolStopped)
	require.NoError(t, <-stopped)
	_, exists := base.record(record.PreparationID)
	assert.False(t, exists)
	assert.True(t, rt.wasRemoved(record.RuntimeID))
}

func TestFUSEPoolColdAcquireDoesNotReturnReservationCommittedAfterStop(t *testing.T) {
	rt := newFUSEMockRuntime()
	base := newMemoryFUSEPoolRepository()
	repo := &blockingAfterTransitionRepository{memoryFUSEPoolRepository: base, entered: make(chan struct{}), release: make(chan struct{})}
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 0, 1
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	type result struct {
		record *state.FUSEPoolRecord
		err    error
	}
	acquired := make(chan result, 1)
	go func() {
		record, err := pool.Acquire(context.Background(), "pool-key")
		acquired <- result{record: record, err: err}
	}()
	<-repo.entered
	stopped := make(chan error, 1)
	go func() { stopped <- pool.Stop(context.Background()) }()
	require.Eventually(t, func() bool {
		pool.lifecycleMu.Lock()
		defer pool.lifecycleMu.Unlock()
		return pool.stopping
	}, time.Second, time.Millisecond)
	close(repo.release)
	got := <-acquired
	assert.Nil(t, got.record)
	require.ErrorIs(t, got.err, ErrFUSEPoolStopped)
	require.NoError(t, <-stopped)
	records, err := base.ListByPoolKey(context.Background(), "pool-key")
	require.NoError(t, err)
	assert.Empty(t, records)
	assert.True(t, rt.wasRemoved("prepared-runtime-1"))
}

func TestFUSEPoolExpiredPreparingRequiresDeletableDisposition(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition FUSEPoolDisposition
		guardErr    error
	}{
		{name: "protected", disposition: FUSEPoolProtected},
		{name: "unknown", disposition: FUSEPoolDispositionUnknown, guardErr: errors.New("owner unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFUSEMockRuntime()
			repo := newMemoryFUSEPoolRepository()
			record := state.FUSEPoolRecord{RuntimeID: "runtime-a", RuntimeUID: "uid-a", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-a", PrepareUntil: repo.now.Add(-time.Second), UpdatedAt: repo.now.Add(-time.Minute), Revision: 1}
			repo.seed(record)
			rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
			cfg := fusePoolConfig()
			cfg.MinSize = 0
			cfg.PristineGuard = func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
				return tc.disposition, tc.guardErr
			}
			pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
			require.NoError(t, pool.Reconcile(context.Background()))
			_, exists := repo.record(record.RuntimeUID)
			assert.True(t, exists)
			assert.False(t, rt.wasRemoved(record.RuntimeID))
		})
	}
}

func TestFUSEPoolReleaseConsumedRemovesRuntimeBeforeRecord(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	require.NoError(t, pool.WarmUp(context.Background()))
	record, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	binding, err := repo.Transition(context.Background(), record.PreparationID, state.FUSEPoolReserved, state.FUSEPoolBinding, record.ReservationToken, record.Revision)
	require.NoError(t, err)
	consumed, err := repo.Transition(context.Background(), binding.PreparationID, state.FUSEPoolBinding, state.FUSEPoolConsumed, binding.ReservationToken, binding.Revision)
	require.NoError(t, err)
	rt.failRemove(consumed.RuntimeID, errors.New("remove failed"))

	err = pool.ReleaseConsumed(context.Background(), *consumed)
	require.Error(t, err)
	_, exists := repo.record(consumed.PreparationID)
	assert.True(t, exists, "record must remain retryable while runtime removal fails")

	rt.failRemove(consumed.RuntimeID, nil)
	repo.failNext("DeleteCleanup", state.ErrFUSEPoolCASMismatch)
	require.ErrorIs(t, pool.ReleaseConsumed(context.Background(), *consumed), state.ErrFUSEPoolCASMismatch)
	_, exists = repo.record(consumed.PreparationID)
	assert.True(t, exists, "a failed conditional delete must remain retryable")

	require.NoError(t, pool.ReleaseConsumed(context.Background(), *consumed))
	_, exists = repo.record(consumed.PreparationID)
	assert.False(t, exists)
}

func TestFUSEPoolReconcileCleansUnsafeStatesAndLeavesConsumed(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	cfg.PrepareTimeout = 50 * time.Millisecond
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	old := repo.now.Add(-time.Hour)
	records := []state.FUSEPoolRecord{
		{RuntimeID: "preparing", RuntimeUID: "uid-preparing", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-b", PrepareUntil: old, UpdatedAt: old, Revision: 1},
		{RuntimeID: "cold-preparing", RuntimeUID: "uid-cold-preparing", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-b", ReservationToken: "cold", ReservedUntil: old, PrepareUntil: old, UpdatedAt: repo.now, Revision: 1},
		{RuntimeID: "reserved", RuntimeUID: "uid-reserved", PoolKey: "pool-key", State: state.FUSEPoolReserved, MaintainerToken: "api-b", ReservationToken: "r", ReservedUntil: old, UpdatedAt: old, Revision: 2},
		{RuntimeID: "binding", RuntimeUID: "uid-binding", PoolKey: "pool-key", State: state.FUSEPoolBinding, MaintainerToken: "api-b", ReservationToken: "r", ReservedUntil: old, UpdatedAt: old, Revision: 3},
		{RuntimeID: "consumed", RuntimeUID: "uid-consumed", PoolKey: "pool-key", State: state.FUSEPoolConsumed, MaintainerToken: "api-b", ReservationToken: "r", ReservedUntil: old, UpdatedAt: old, Revision: 4},
		{RuntimeID: "other", RuntimeUID: "uid-other", PoolKey: "other-key", State: state.FUSEPoolBinding, MaintainerToken: "api-b", ReservationToken: "r", ReservedUntil: old, UpdatedAt: old, Revision: 2},
	}
	repo.seed(records...)
	for _, record := range records {
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID, State: "running"}
	}

	require.NoError(t, pool.Reconcile(context.Background()))
	for _, uid := range []string{"uid-preparing", "uid-cold-preparing", "uid-reserved"} {
		_, exists := repo.record(uid)
		assert.False(t, exists, uid)
	}
	_, consumedExists := repo.record("uid-consumed")
	_, bindingExists := repo.record("uid-binding")
	_, otherExists := repo.record("uid-other")
	assert.True(t, consumedExists)
	assert.True(t, bindingExists, "binding is not deletable without an explicit abandoned disposition")
	assert.True(t, otherExists)
}

func TestFUSEPoolMaxCountsOnlyPreparingAndPrepared(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	for i := 0; i < 20; i++ {
		repo.seed(state.FUSEPoolRecord{
			RuntimeID: "used-" + string(rune('a'+i)), RuntimeUID: "used-uid-" + string(rune('a'+i)), PoolKey: "pool-key",
			State: state.FUSEPoolConsumed, MaintainerToken: "api-b", ReservationToken: "r", ReservedUntil: time.Now().Add(time.Hour), UpdatedAt: time.Now(), Revision: 5,
		})
	}
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 2, 2
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	require.NoError(t, pool.WarmUp(context.Background()))
	assert.Equal(t, 2, repo.countState("pool-key", state.FUSEPoolPrepared))
	assert.Equal(t, 2, rt.prepareCount())
}

func TestFUSEPoolReconcileTrimsPreparedAboveMax(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	now := time.Now()
	for _, suffix := range []string{"a", "b", "c"} {
		repo.seed(state.FUSEPoolRecord{
			RuntimeID: "prepared-" + suffix, RuntimeUID: "prepared-" + suffix, PoolKey: "pool-key",
			State: state.FUSEPoolPrepared, MaintainerToken: "api-b", UpdatedAt: now, Revision: 2,
		})
	}
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 0, 1
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	require.NoError(t, pool.Reconcile(context.Background()))
	assert.Equal(t, 1, repo.countState("pool-key", state.FUSEPoolPrepared))
}

func TestFUSEPoolReconcileKeepsRecordOnRemovalFailureAndSkipsStaleCAS(t *testing.T) {
	makeBinding := func(runtimeID string) state.FUSEPoolRecord {
		return state.FUSEPoolRecord{
			RuntimeID: runtimeID, RuntimeUID: "uid-" + runtimeID, PoolKey: "pool-key", State: state.FUSEPoolBinding,
			MaintainerToken: "api-b", ReservationToken: "reservation", ReservedUntil: time.Now().Add(time.Minute), UpdatedAt: time.Now(), Revision: 3,
		}
	}
	t.Run("runtime removal failure", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		record := makeBinding("remove-fails")
		repo.seed(record)
		rt.failRemove(record.RuntimeID, errors.New("remove failed"))
		cfg := fusePoolConfig()
		cfg.MinSize = 0
		cfg.PristineGuard = func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
			return FUSEPoolAbandoned, nil
		}
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		require.Error(t, pool.Reconcile(context.Background()))
		_, exists := repo.record(record.RuntimeUID)
		assert.True(t, exists)
	})

	t.Run("stale conditional delete", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		repo := newMemoryFUSEPoolRepository()
		record := makeBinding("cas-race")
		repo.seed(record)
		repo.failNext("DeleteCleanup", state.ErrFUSEPoolCASMismatch)
		cfg := fusePoolConfig()
		cfg.MinSize = 0
		cfg.PristineGuard = func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
			return FUSEPoolAbandoned, nil
		}
		pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

		require.NoError(t, pool.Reconcile(context.Background()))
		_, exists := repo.record(record.RuntimeUID)
		assert.True(t, exists, "a stale reconciler must not delete a newer record")
		assert.True(t, rt.wasRemoved(record.RuntimeID))
	})
}

func TestFUSEPoolConcurrentReconcilersUsePoolKeyLock(t *testing.T) {
	repo := newMemoryFUSEPoolRepository()
	rtA, rtB := newFUSEMockRuntime(), newFUSEMockRuntime()
	cfgA := fusePoolConfig()
	cfgA.MinSize, cfgA.MaxSize = 2, 2
	cfgB := cfgA
	cfgB.MaintainerToken = "api-b"
	pools := []*FUSEPool{
		NewFUSEPool(rtA, repo, cfgA, fixedFUSESpec("pool-key")),
		NewFUSEPool(rtB, repo, cfgB, fixedFUSESpec("pool-key")),
	}
	entered, release := rtA.blockPrepare()
	errA := make(chan error, 1)
	go func() { errA <- pools[0].Reconcile(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first reconciler did not enter preparation")
	}
	errB := make(chan error, 1)
	go func() { errB <- pools[1].Reconcile(context.Background()) }()
	select {
	case err := <-errB:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("second reconciler did not observe held lock")
	}
	close(release)
	select {
	case err := <-errA:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("first reconciler did not finish")
	}
	assert.Equal(t, 2, repo.countPreparingAndPrepared("pool-key"))
	assert.Equal(t, 2, rtA.prepareCount()+rtB.prepareCount())
}

func TestFUSEPoolDrainRemovesOnlyPreparedForExactPoolKey(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	now := time.Now()
	records := []state.FUSEPoolRecord{
		{RuntimeID: "prepared-a", RuntimeUID: "prepared-a", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: now, Revision: 2},
		{RuntimeID: "preparing-a", RuntimeUID: "preparing-a", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-a", UpdatedAt: now, Revision: 1},
		{RuntimeID: "prepared-other", RuntimeUID: "prepared-other", PoolKey: "other-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: now, Revision: 2},
	}
	repo.seed(records...)
	for _, record := range records {
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID, State: "running"}
	}

	require.NoError(t, pool.Drain(context.Background(), "pool-key"))
	_, prepared := repo.record("prepared-a")
	_, preparing := repo.record("preparing-a")
	_, other := repo.record("prepared-other")
	assert.False(t, prepared)
	assert.True(t, preparing)
	assert.True(t, other)
}

func TestFUSEPoolDrainReleaseResumesInProgressCleanup(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	record := state.FUSEPoolRecord{
		PreparationID:   "cleanup-from-stopped-api",
		RuntimeID:       "runtime-from-stopped-api",
		RuntimeUID:      "uid-from-stopped-api",
		PoolKey:         "former-pool-key",
		State:           state.FUSEPoolCleanup,
		MaintainerToken: "former-api",
		CleanupToken:    "former-api:cleanup:record",
		CleanupUntil:    repo.now.Add(time.Minute),
		UpdatedAt:       repo.now,
		Revision:        4,
	}
	repo.seed(record)
	rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))

	require.NoError(t, pool.DrainRelease(context.Background()))
	_, exists := repo.record(record.PreparationID)
	assert.False(t, exists)
	assert.True(t, rt.wasRemoved(record.RuntimeID))
}

func TestFUSEPoolDrainAndStopAreScoped(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	now := time.Now()
	records := []state.FUSEPoolRecord{
		{RuntimeID: "own-preparing", RuntimeUID: "own-preparing", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-a", UpdatedAt: now, Revision: 1},
		{RuntimeID: "own-prepared", RuntimeUID: "own-prepared", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: now, Revision: 2},
		{RuntimeID: "other-prepared", RuntimeUID: "other-prepared", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-b", UpdatedAt: now, Revision: 2},
		{RuntimeID: "own-reserved", RuntimeUID: "own-reserved", PoolKey: "pool-key", State: state.FUSEPoolReserved, MaintainerToken: "api-a", ReservationToken: "r", ReservedUntil: now.Add(time.Hour), UpdatedAt: now, Revision: 3},
	}
	repo.seed(records...)
	for _, record := range records {
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID, State: "running"}
	}

	require.NoError(t, pool.Stop(context.Background()))
	require.NoError(t, pool.Stop(context.Background()))
	_, ownPreparing := repo.record("own-preparing")
	_, ownPrepared := repo.record("own-prepared")
	_, otherPrepared := repo.record("other-prepared")
	_, ownReserved := repo.record("own-reserved")
	assert.False(t, ownPreparing)
	assert.False(t, ownPrepared)
	assert.True(t, otherPrepared)
	assert.True(t, ownReserved)
}

func TestFUSEPoolStopRetainsProtectedAndUnknownOwnedRecords(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	now := repo.now
	records := []state.FUSEPoolRecord{
		{PreparationID: "protected", RuntimeID: "protected", RuntimeUID: "uid-protected", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-a", UpdatedAt: now, Revision: 2},
		{PreparationID: "unknown", RuntimeID: "unknown", RuntimeUID: "uid-unknown", PoolKey: "pool-key", State: state.FUSEPoolPreparing, MaintainerToken: "api-a", PrepareUntil: now.Add(time.Minute), UpdatedAt: now, Revision: 1},
		{PreparationID: "other", RuntimeID: "other", RuntimeUID: "uid-other", PoolKey: "pool-key", State: state.FUSEPoolPrepared, MaintainerToken: "api-b", UpdatedAt: now, Revision: 2},
	}
	repo.seed(records...)
	for _, record := range records {
		rt.sandboxes[record.RuntimeID] = &runtime.SandboxInfo{RuntimeID: record.RuntimeID, RuntimeUID: record.RuntimeUID}
	}
	cfg := fusePoolConfig()
	cfg.PristineGuard = func(_ context.Context, record state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
		switch record.PreparationID {
		case "protected":
			return FUSEPoolProtected, nil
		case "unknown":
			return FUSEPoolDispositionUnknown, errors.New("owner lookup unavailable")
		default:
			return FUSEPoolPristine, nil
		}
	}
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))

	err := pool.Stop(context.Background())
	require.ErrorIs(t, err, ErrFUSEPoolProtected)
	require.ErrorIs(t, err, ErrFUSEPoolReturnUnproven)
	for _, record := range records {
		_, exists := repo.record(record.PreparationID)
		assert.True(t, exists, record.PreparationID)
		assert.False(t, rt.wasRemoved(record.RuntimeID), record.PreparationID)
	}
}

func TestFUSEPoolStopCancelsInFlightRefill(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	require.NoError(t, pool.WarmUp(context.Background()))
	entered, _ := rt.blockPrepare()
	_, err := pool.Acquire(context.Background(), "pool-key")
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refill did not enter preparation")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- pool.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the in-flight refill")
	}
}

func TestFUSEPoolStopCancelsInFlightAcquireWithoutReservedLeak(t *testing.T) {
	rt := newFUSEMockRuntime()
	entered, _ := rt.blockPrepare()
	repo := newMemoryFUSEPoolRepository()
	cfg := fusePoolConfig()
	cfg.MinSize = 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-key"))
	acquired := make(chan error, 1)
	go func() { _, err := pool.Acquire(context.Background(), "pool-key"); acquired <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("acquire did not enter preparation")
	}
	require.NoError(t, pool.Stop(context.Background()))
	select {
	case err := <-acquired:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("acquire did not stop")
	}
	assert.Equal(t, 0, repo.countState("pool-key", state.FUSEPoolReserved))
}

func TestFUSEPoolConcurrentStartWaitsForSameInitialReconcile(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	require.True(t, func() bool {
		ok, _ := repo.TryRefillLock(context.Background(), "pool-key", "other", time.Minute)
		return ok
	}())
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	results := make(chan error, 2)
	go func() { results <- pool.Start(context.Background()) }()
	go func() { results <- pool.Start(context.Background()) }()
	select {
	case err := <-results:
		t.Fatalf("Start returned before the busy initial refill lock was released: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, repo.UnlockRefill(context.Background(), "pool-key", "other"))
	for range 2 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("Start did not finish")
		}
	}
	require.True(t, pool.Running())
	require.NoError(t, pool.Stop(context.Background()))
}

func TestFUSEPoolStartWaiterReelectsAfterLeaderContextCanceled(t *testing.T) {
	rt := newFUSEMockRuntime()
	repo := newMemoryFUSEPoolRepository()
	require.True(t, func() bool {
		ok, _ := repo.TryRefillLock(context.Background(), "pool-key", "other", time.Minute)
		return ok
	}())
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan error, 1)
	waiter := make(chan error, 1)
	go func() { leader <- pool.Start(leaderCtx) }()
	require.Eventually(t, func() bool { pool.lifecycleMu.Lock(); defer pool.lifecycleMu.Unlock(); return pool.starting }, time.Second, time.Millisecond)
	go func() { waiter <- pool.Start(context.Background()) }()
	require.Eventually(t, func() bool {
		pool.lifecycleMu.Lock()
		defer pool.lifecycleMu.Unlock()
		return pool.startWaiters == 1
	}, time.Second, time.Millisecond)
	cancelLeader()
	select {
	case err := <-leader:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("leader did not cancel")
	}
	require.NoError(t, repo.UnlockRefill(context.Background(), "pool-key", "other"))
	select {
	case err := <-waiter:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("live waiter did not re-elect")
	}
	assert.True(t, pool.Running())
	require.NoError(t, pool.Stop(context.Background()))
}

func TestFUSEPoolStopCancelsBlockedInitialStart(t *testing.T) {
	rt := newFUSEMockRuntime()
	entered, _ := rt.blockPrepare()
	repo := newMemoryFUSEPoolRepository()
	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	started := make(chan error, 1)
	go func() { started <- pool.Start(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("initial Start did not enter prepare")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, pool.Stop(stopCtx))
	select {
	case err := <-started:
		require.ErrorIs(t, err, ErrFUSEPoolStopped)
	case <-time.After(time.Second):
		t.Fatal("Start remained blocked after Stop")
	}
	assert.Equal(t, 0, repo.countState("pool-key", state.FUSEPoolPreparing))
	assert.Equal(t, 0, repo.countState("pool-key", state.FUSEPoolReserved))
}

func TestFUSEPoolStartFailsClosed(t *testing.T) {
	t.Run("missing pool", func(t *testing.T) {
		mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{DefaultMountMode: WorkspaceMountFUSE, EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true}})
		require.ErrorIs(t, mgr.Start(context.Background()), ErrInvalidFUSEPoolConfig)
	})

	t.Run("redis reconciliation", func(t *testing.T) {
		repo := newMemoryFUSEPoolRepository()
		repo.failNext("TryRefillLock", errors.New("redis unavailable"))
		pool := NewFUSEPool(newFUSEMockRuntime(), repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
		mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{DefaultMountMode: WorkspaceMountFUSE, EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true}, FUSEPool: pool})

		err := mgr.Start(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redis unavailable")
		assert.False(t, pool.Running())
	})

	t.Run("redis server time", func(t *testing.T) {
		repo := newMemoryFUSEPoolRepository()
		repo.failNext("ServerTime", errors.New("redis time unavailable"))
		pool := NewFUSEPool(newFUSEMockRuntime(), repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
		err := pool.Start(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "redis time unavailable")
		assert.False(t, pool.Running())
	})

	t.Run("inventory count", func(t *testing.T) {
		repo := newMemoryFUSEPoolRepository()
		repo.failNext("CountPreparingAndPrepared", errors.New("count unavailable"))
		pool := NewFUSEPool(newFUSEMockRuntime(), repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
		mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{DefaultMountMode: WorkspaceMountFUSE, EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true}, FUSEPool: pool})

		err := mgr.Start(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "count unavailable")
		assert.False(t, pool.Running())
	})

	t.Run("warm preparation", func(t *testing.T) {
		rt := newFUSEMockRuntime()
		rt.prepareErr = errors.New("runtime unavailable")
		pool := NewFUSEPool(rt, newMemoryFUSEPoolRepository(), fusePoolConfig(), fixedFUSESpec("pool-key"))
		mgr := NewManager(rt, nil, nil, ManagerConfig{DefaultMountMode: WorkspaceMountFUSE, EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true}, FUSEPool: pool})

		err := mgr.Start(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "runtime unavailable")
		assert.False(t, pool.Running())
	})

	t.Run("persistent restore", func(t *testing.T) {
		store := newAtomicMemoryStore()
		store.failNext("Keys", errors.New("state unavailable"))
		pool := NewFUSEPool(newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), fusePoolConfig(), fixedFUSESpec("pool-key"))
		mgr := NewManager(newMockRuntime(), nil, nil, ManagerConfig{DefaultMountMode: WorkspaceMountFUSE, EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true}, FUSEPool: pool})
		mgr.SetSessionStore(NewSessionStore(store, time.Minute))

		err := mgr.Start(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "state unavailable")
		assert.False(t, pool.Running())
	})
}

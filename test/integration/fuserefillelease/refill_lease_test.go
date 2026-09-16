package fuserefillelease_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const refillLeaseTTL = 45 * time.Second

type deployedConfig struct {
	apiURL           string
	apiKey           string
	poolKey          string
	redisAddr        string
	redisPassword    string
	expectedCapacity int
}

type sandboxClient struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func deployedTestConfig(t *testing.T) deployedConfig {
	t.Helper()
	if os.Getenv("SANDBOX_REFILL_LEASE_TEST") != "1" {
		t.Skip("opt-in deployed FUSE refill lease test")
	}
	cfg := deployedConfig{
		apiURL:        strings.TrimRight(os.Getenv("SANDBOX_REFILL_TEST_API_URL"), "/"),
		apiKey:        os.Getenv("SANDBOX_REFILL_TEST_API_KEY"),
		poolKey:       os.Getenv("SANDBOX_REFILL_TEST_POOL_KEY"),
		redisAddr:     os.Getenv("SANDBOX_REFILL_TEST_REDIS_ADDR"),
		redisPassword: os.Getenv("SANDBOX_REFILL_TEST_REDIS_PASSWORD"),
	}
	require.NotEmpty(t, cfg.apiURL)
	require.NotEmpty(t, cfg.redisAddr)
	parsed, err := url.Parse(cfg.apiURL)
	require.NoError(t, err)
	require.Contains(t, []string{"http", "https"}, parsed.Scheme)
	require.NotEmpty(t, parsed.Host)
	require.Empty(t, parsed.User, "API credentials belong in SANDBOX_REFILL_TEST_API_KEY")
	cfg.expectedCapacity = 3
	if raw := os.Getenv("SANDBOX_REFILL_TEST_EXPECTED_CAPACITY"); raw != "" {
		cfg.expectedCapacity, err = strconv.Atoi(raw)
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, cfg.expectedCapacity, 1)
	require.LessOrEqual(t, cfg.expectedCapacity, 100)
	return cfg
}

func newRedisStore(t *testing.T, address, password string, durability redisstate.DurabilityMode) *redisstate.Store {
	t.Helper()
	store, err := redisstate.New(context.Background(), redisstate.Options{
		Mode:        redisstate.ModeStandalone,
		Addr:        address,
		Password:    password,
		Durability:  durability,
		AckReplicas: 1,
		AckTimeout:  time.Second,
		DialTimeout: 3 * time.Second,
		ReadTimeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func preparedCount(ctx context.Context, repository *redisstate.FUSEPoolRepository, poolKey string) (int, error) {
	records, err := repository.ListByPoolKey(ctx, poolKey)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, record := range records {
		if record.State == state.FUSEPoolPrepared {
			count++
		}
	}
	return count, nil
}

func requirePristinePool(t *testing.T, ctx context.Context, repository *redisstate.FUSEPoolRepository, poolKey string, capacity int) {
	t.Helper()
	records, err := repository.ListByPoolKey(ctx, poolKey)
	require.NoError(t, err)
	require.Len(t, records, capacity, "test requires an idle pool with no active or cleanup records")
	for _, record := range records {
		require.Equal(t, state.FUSEPoolPrepared, record.State, "test requires every target pool member to be prepared")
	}
}

func selectPoolKey(t *testing.T, ctx context.Context, repository *redisstate.FUSEPoolRepository, configured string, capacity int) string {
	t.Helper()
	if configured != "" {
		requirePristinePool(t, ctx, repository, configured, capacity)
		return configured
	}
	poolKeys, err := repository.ListPoolKeys(ctx)
	require.NoError(t, err)
	var candidates []string
	for _, poolKey := range poolKeys {
		records, listErr := repository.ListByPoolKey(ctx, poolKey)
		require.NoError(t, listErr)
		if len(records) != capacity {
			continue
		}
		eligible := true
		for _, record := range records {
			if record.State != state.FUSEPoolPrepared {
				eligible = false
				break
			}
		}
		if eligible {
			candidates = append(candidates, poolKey)
		}
	}
	require.Len(t, candidates, 1, "set SANDBOX_REFILL_TEST_POOL_KEY when zero or multiple pristine FUSE pools are present")
	return candidates[0]
}

func (c *sandboxClient) request(ctx context.Context, method, path string, input, output any) (status int, err error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return 0, err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, response.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return response.StatusCode, err
	}
	if len(data) > 2<<20 {
		return response.StatusCode, errors.New("API response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("%s %s: status=%d body=%s", method, path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

func createFUSESandbox(t *testing.T, ctx context.Context, client *sandboxClient) string {
	t.Helper()
	var response types.SandboxResponse
	status, err := client.request(ctx, http.MethodPost, "/api/v1/sandboxes", types.CreateSandboxRequest{
		Mode:               "persistent",
		Timeout:            300,
		WorkspacePath:      "validation/refill-lease/" + uuid.NewString(),
		WorkspaceMountMode: types.WorkspaceMountFUSE,
	}, &response)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, status)
	require.NotEmpty(t, response.ID)
	require.Equal(t, string(types.WorkspaceMountFUSE), response.WorkspaceMountMode)
	return response.ID
}

func destroySandbox(ctx context.Context, client *sandboxClient, sandboxID string) error {
	status, err := client.request(ctx, http.MethodDelete, "/api/v1/sandboxes/"+sandboxID, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("destroy sandbox: status=%d", status)
	}
	return nil
}

func TestDeployedFUSERefillLeaseTakeover(t *testing.T) {
	cfg := deployedTestConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store := newRedisStore(t, cfg.redisAddr, cfg.redisPassword, redisstate.DurabilityReplicaAck)
	repository := redisstate.NewFUSEPoolRepository(store)
	cfg.poolKey = selectPoolKey(t, ctx, repository, cfg.poolKey, cfg.expectedCapacity)

	oldToken := "acceptance-old-" + uuid.NewString()
	var locked bool
	var lockErr error
	require.Eventually(t, func() bool {
		locked, lockErr = repository.TryRefillLock(ctx, cfg.poolKey, oldToken, refillLeaseTTL)
		return lockErr == nil && locked
	}, 20*time.Second, time.Second, "a stable full pool never released its transient refill owner; aborting before sandbox creation")
	lockedAt := time.Now()
	requirePristinePool(t, ctx, repository, cfg.poolKey, cfg.expectedCapacity)

	client := &sandboxClient{baseURL: cfg.apiURL, apiKey: cfg.apiKey, client: &http.Client{Timeout: 90 * time.Second}}
	sandboxID := createFUSESandbox(t, ctx, client)
	destroyed := false
	t.Cleanup(func() {
		if destroyed {
			return
		}
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		if err := destroySandbox(cleanupContext, client, sandboxID); err != nil {
			t.Errorf("cleanup test sandbox %s: %v", sandboxID, err)
		}
	})

	count, err := preparedCount(ctx, repository, cfg.poolKey)
	require.NoError(t, err)
	require.Less(t, count, cfg.expectedCapacity)
	for time.Now().Before(lockedAt.Add(40 * time.Second)) {
		count, err = preparedCount(ctx, repository, cfg.poolKey)
		require.NoError(t, err)
		require.Less(t, count, cfg.expectedCapacity, "pool refilled before the old lease expired")
		time.Sleep(time.Second)
	}
	remaining := time.Until(lockedAt.Add(90 * time.Second))
	require.Positive(t, remaining)
	require.Eventually(t, func() bool {
		observed, observeErr := preparedCount(ctx, repository, cfg.poolKey)
		return observeErr == nil && observed == cfg.expectedCapacity
	}, remaining, time.Second, "active API did not take over and refill after the 45-second lease expired")

	renewed, err := repository.RenewRefillLock(ctx, cfg.poolKey, oldToken, refillLeaseTTL)
	require.NoError(t, err)
	require.False(t, renewed, "expired owner regained the refill lease")
	require.NoError(t, destroySandbox(ctx, client, sandboxID))
	destroyed = true
	require.Eventually(t, func() bool {
		observed, observeErr := preparedCount(ctx, repository, cfg.poolKey)
		return observeErr == nil && observed == cfg.expectedCapacity
	}, 60*time.Second, time.Second)
}

func TestRealRedisRefillLeaseFencing(t *testing.T) {
	if os.Getenv("SANDBOX_REFILL_LEASE_REDIS_TEST") != "1" {
		t.Skip("opt-in 45-second real Redis lease test")
	}
	address := os.Getenv("TEST_REDIS_ADDR")
	require.NotEmpty(t, address)
	password := os.Getenv("TEST_REDIS_PASSWORD")
	storeA := newRedisStore(t, address, password, redisstate.DurabilityBestEffort)
	storeB := newRedisStore(t, address, password, redisstate.DurabilityBestEffort)
	repositoryA := redisstate.NewFUSEPoolRepository(storeA)
	repositoryB := redisstate.NewFUSEPoolRepository(storeB)
	poolKey := "acceptance/refill-lease/" + uuid.NewString()
	ownerA := "owner-a-" + uuid.NewString()
	ownerB := "owner-b-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()

	locked, err := repositoryA.TryRefillLock(ctx, poolKey, ownerA, refillLeaseTTL)
	require.NoError(t, err)
	require.True(t, locked)
	locked, err = repositoryB.TryRefillLock(ctx, poolKey, ownerB, refillLeaseTTL)
	require.NoError(t, err)
	require.False(t, locked)
	require.Eventually(t, func() bool {
		acquired, acquireErr := repositoryB.TryRefillLock(ctx, poolKey, ownerB, refillLeaseTTL)
		locked, err = acquired, acquireErr
		return acquireErr == nil && acquired
	}, 55*time.Second, time.Second)
	t.Cleanup(func() {
		if locked {
			_ = repositoryB.UnlockRefill(context.Background(), poolKey, ownerB)
		}
	})
	renewed, err := repositoryA.RenewRefillLock(ctx, poolKey, ownerA, refillLeaseTTL)
	require.NoError(t, err)
	require.False(t, renewed)
	require.ErrorIs(t, repositoryA.UnlockRefill(ctx, poolKey, ownerA), state.ErrFUSEPoolTokenMismatch)
	require.NoError(t, repositoryB.UnlockRefill(ctx, poolKey, ownerB))
	locked = false
}

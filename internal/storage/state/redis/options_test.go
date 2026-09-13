package redis

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestUniversalOptionsSentinelCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, username, password string
	}{
		{name: "unset"},
		{name: "password only", password: "sentinel password\\\""},
		{name: "username only", username: "sentinel-user"},
		{name: "both", username: "sentinel-user", password: "sentinel password\\\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := universalOptions(Options{
				Mode: ModeSentinel, Addrs: []string{"sentinel-a:26379", "sentinel-b:26379"},
				MasterName: "sandbox-master", Username: "data-user", Password: "data-password",
				SentinelUsername: tc.username, SentinelPassword: tc.password,
			})
			require.NoError(t, err)
			require.Equal(t, tc.username, got.SentinelUsername)
			require.Equal(t, tc.password, got.SentinelPassword)
			require.Equal(t, "data-user", got.Username)
			require.Equal(t, "data-password", got.Password)
			require.Equal(t, "sandbox-master", got.MasterName)
			require.Equal(t, []string{"sentinel-a:26379", "sentinel-b:26379"}, got.Addrs)
		})
	}
}

func TestUniversalOptionsDefaultStandalone(t *testing.T) {
	got, err := universalOptions(Options{Addr: "redis:6379"})
	require.NoError(t, err)
	require.Equal(t, &goredis.UniversalOptions{Addrs: []string{"redis:6379"}}, got)
}

func TestUniversalOptionsConnectionTuning(t *testing.T) {
	addrs := []string{"redis-a:6379", "redis-b:6379"}
	got, err := universalOptions(Options{
		Mode: ModeCluster, Addr: "ignored:6379", Addrs: addrs,
		Username: "data-user", Password: "data-password", PoolSize: 17,
		MinIdleConns: 3, DialTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 4 * time.Second, MaxRetries: 5,
	})
	require.NoError(t, err)
	require.Equal(t, &goredis.UniversalOptions{
		IsClusterMode: true,
		Addrs:         []string{"redis-a:6379", "redis-b:6379"}, Username: "data-user",
		Password: "data-password", PoolSize: 17, MinIdleConns: 3,
		DialTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second,
		WriteTimeout: 4 * time.Second, MaxRetries: 5,
	}, got)
	addrs[0] = "mutated:6379"
	require.Equal(t, "redis-a:6379", got.Addrs[0])
}

func TestUniversalOptionsStandaloneDatabase(t *testing.T) {
	got, err := universalOptions(Options{Mode: ModeStandalone, Addr: "redis:6379", DB: 4})
	require.NoError(t, err)
	require.Equal(t, 4, got.DB)
}

func TestUniversalOptionsTopologyValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no address", Options{}, "redis: at least one address is required"},
		{"default standalone multiple", Options{Addrs: []string{"a:6379", "b:6379"}}, "redis: standalone mode requires exactly one address"},
		{"explicit standalone multiple", Options{Mode: ModeStandalone, Addrs: []string{"a:6379", "b:6379"}}, "redis: standalone mode requires exactly one address"},
		{"sentinel master missing", Options{Mode: ModeSentinel, Addr: "a:26379"}, "redis: sentinel mode requires a master name"},
		{"cluster database", Options{Mode: ModeCluster, Addr: "a:6379", DB: 1}, "redis: cluster mode supports database 0 only"},
		{"unsupported mode", Options{Mode: "unknown", Addr: "a:6379"}, "redis: unsupported mode \"unknown\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := universalOptions(tc.opts)
			require.EqualError(t, err, tc.want)
			require.Nil(t, got)
		})
	}
}

func TestUniversalOptionsExplicitClusterWithOneSeed(t *testing.T) {
	opts, err := universalOptions(Options{Mode: ModeCluster, Addr: "seed:6379"})
	require.NoError(t, err)
	client := goredis.NewUniversalClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	require.IsType(t, &goredis.ClusterClient{}, client)
}

func TestUniversalOptionsRejectsConflictingMasterName(t *testing.T) {
	for _, mode := range []Mode{"", ModeStandalone, ModeCluster} {
		opts, err := universalOptions(Options{Mode: mode, Addr: "seed:6379", MasterName: "leftover"})
		require.EqualError(t, err, "redis: master name is only valid in sentinel mode")
		require.Nil(t, opts)
	}
}

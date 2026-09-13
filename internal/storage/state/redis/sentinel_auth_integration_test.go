package redis

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	fixture "github.com/goairix/sandbox/testdata/sentinel-auth"
)

func TestSentinelAuthIntegration(t *testing.T) {
	addrs := os.Getenv("TEST_REDIS_SENTINEL_AUTH_ADDRS")
	if addrs == "" {
		t.Skip("isolated Sentinel authentication fixture not enabled")
	}
	require.Equal(t, fixture.SentinelAddrs, addrs, "only the isolated fixture addresses are accepted")
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	data := make([]*goredis.Client, 3)
	sentinels := make([]*goredis.SentinelClient, 3)
	for i := range data {
		data[i] = goredis.NewClient(&goredis.Options{
			Addr: fmt.Sprintf("redis-%d:6379", i), Username: fixture.DataUsername,
			Password: fixture.DataPassword, DialTimeout: time.Second,
			ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1,
		})
		sentinels[i] = goredis.NewSentinelClient(&goredis.Options{
			Addr: fmt.Sprintf("sentinel-%d:26379", i), Username: fixture.SentinelUsername,
			Password: fixture.SentinelPassword, DialTimeout: time.Second,
			ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1,
		})
		index := i
		t.Cleanup(func() {
			require.NoError(t, data[index].Close())
			require.NoError(t, sentinels[index].Close())
		})
	}
	deadline := time.Now().Add(40 * time.Second)
	for !sentinelAuthTopologyHealthy(ctx, data, sentinels) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatal("authenticated replication and Sentinel peer topology did not converge")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Log("four authentication links converged: replication, Sentinel-to-Redis, client-to-Sentinel, Sentinel peers")
	opts := Options{
		Mode: ModeSentinel, Addrs: strings.Split(addrs, ","), MasterName: fixture.MasterName,
		Username: fixture.DataUsername, Password: fixture.DataPassword,
		SentinelUsername: fixture.SentinelUsername, SentinelPassword: fixture.SentinelPassword,
		Durability: DurabilityReplicaAck, AckReplicas: 1, AckTimeout: 3 * time.Second,
		DialTimeout: time.Second, ReadTimeout: 4 * time.Second, WriteTimeout: time.Second,
		MaxRetries: -1,
	}
	t.Run("negative_direct_credentials", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			for _, target := range []struct {
				addr, username, wrongPassword string
			}{
				{fmt.Sprintf("redis-%d:6379", i), fixture.DataUsername, fixture.SentinelPassword},
				{fmt.Sprintf("sentinel-%d:26379", i), fixture.SentinelUsername, fixture.DataPassword},
			} {
				for _, authenticated := range []bool{false, true} {
					clientOpts := &goredis.Options{Addr: target.addr, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1}
					if authenticated {
						clientOpts.Username, clientOpts.Password = target.username, target.wrongPassword
					}
					client := goredis.NewClient(clientOpts)
					err := client.Ping(ctx).Err()
					require.NoError(t, client.Close())
					require.Error(t, err, "unauthenticated or cross-end credentials must be rejected")
					require.True(t, strings.Contains(err.Error(), "NOAUTH") || strings.Contains(err.Error(), "WRONGPASS"), "expected explicit authentication denial")
				}
			}
		}
		t.Log("all six endpoints rejected unauthenticated and cross-end passwords")
	})
	for _, endpoint := range []string{"sentinel", "data"} {
		t.Run("negative_"+endpoint+"_store_password", func(t *testing.T) {
			wrong := opts
			if endpoint == "sentinel" {
				wrong.SentinelPassword = fixture.DataPassword
			} else {
				wrong.Password = fixture.SentinelPassword
			}
			checkCtx, checkCancel := context.WithTimeout(ctx, 4*time.Second)
			defer checkCancel()
			store, err := New(checkCtx, wrong)
			if store != nil {
				require.NoError(t, store.Close())
			}
			require.Error(t, err, "production Store must fail with wrong independent password")
			require.Contains(t, err.Error(), "WRONGPASS", "failure must be authentication denial, not an unrelated timeout")
			require.Nil(t, store)
			t.Log("production Store rejected wrong independent " + endpoint + " password")
		})
	}
	t.Run("production_store_write_and_wait", func(t *testing.T) {
		store, err := New(ctx, opts)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		key := fmt.Sprintf("sentinel-auth:%d", time.Now().UnixNano())
		require.NoError(t, store.Set(ctx, key, []byte("replica-ack-write"), time.Minute))
		got, err := store.Get(ctx, key)
		require.NoError(t, err)
		require.Equal(t, []byte("replica-ack-write"), got)
		for i := 1; i < 3; i++ {
			require.Eventually(t, func() bool {
				value, getErr := data[i].Get(ctx, key).Result()
				return getErr == nil && value == "replica-ack-write"
			}, 5*time.Second, 100*time.Millisecond, "both authenticated replicas must contain Store write")
		}
		client, ok := store.client.(*goredis.Client)
		require.True(t, ok, "Sentinel mode must create a data-node client")
		conn := client.Conn()
		defer func() { require.NoError(t, conn.Close()) }()
		require.NoError(t, conn.Set(ctx, key+":wait", "actual-write", time.Minute).Err())
		acked, err := conn.Wait(ctx, 1, 3*time.Second).Result()
		require.NoError(t, err)
		require.GreaterOrEqual(t, acked, int64(1))
		t.Logf("production Store replica_ack Set/Get and both replica reads passed; actual-write same-connection WAIT 1 returned %d", acked)
	})
}

func sentinelAuthTopologyHealthy(ctx context.Context, data []*goredis.Client, sentinels []*goredis.SentinelClient) bool {
	for i, client := range data {
		info, err := client.Info(ctx, "replication").Result()
		if err != nil {
			return false
		}
		if i == 0 {
			if !strings.Contains(info, "role:master\r\n") || !strings.Contains(info, "connected_slaves:2\r\n") || strings.Count(info, "state=online") != 2 {
				return false
			}
		} else if !strings.Contains(info, "role:slave\r\n") || !strings.Contains(info, "master_link_status:up\r\n") {
			return false
		}
	}
	for _, sentinel := range sentinels {
		master, err := sentinel.Master(ctx, fixture.MasterName).Result()
		if err != nil || master["ip"] != "redis-0" || master["num-slaves"] != "2" || master["num-other-sentinels"] != "2" || !sentinelAuthMemberHealthy(master) {
			return false
		}
		replicas, err := sentinel.Replicas(ctx, fixture.MasterName).Result()
		if err != nil || len(replicas) != 2 {
			return false
		}
		for _, replica := range replicas {
			if !sentinelAuthMemberHealthy(replica) || replica["master-link-status"] != "ok" {
				return false
			}
		}
		peers, err := sentinel.Sentinels(ctx, fixture.MasterName).Result()
		if err != nil || len(peers) != 2 {
			return false
		}
		for _, peer := range peers {
			if !sentinelAuthMemberHealthy(peer) || !strings.HasPrefix(peer["ip"], "sentinel-") {
				return false
			}
		}
		quorum, err := sentinel.CkQuorum(ctx, fixture.MasterName).Result()
		if err != nil || !strings.HasPrefix(quorum, "OK 3 usable Sentinels") {
			return false
		}
	}
	return true
}

func sentinelAuthMemberHealthy(member map[string]string) bool {
	flags := member["flags"]
	if flags == "" || strings.Contains(flags, "disconnected") || strings.Contains(flags, "s_down") || strings.Contains(flags, "o_down") {
		return false
	}
	lastOK, err := strconv.ParseInt(member["last-ok-ping-reply"], 10, 64)
	return err == nil && lastOK >= 0 && lastOK < 5000
}

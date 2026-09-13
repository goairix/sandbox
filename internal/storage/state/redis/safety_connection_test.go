package redis

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redislib "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// A minimal RESP server models Redis' per-connection write offset: WAIT can
// acknowledge this test write only on the connection that executed EVALSHA.
type safetyRESPServer struct {
	listener       net.Listener
	mu             sync.Mutex
	connections    []net.Conn
	commands       []string
	writes         []int
	waits          []int
	scriptNoop     bool
	scriptError    string
	onScript       func()
	closeWait      bool
	onWait         func()
	scanPages      map[string]safetyScanPage
	closeScript    bool
	sentinelMaster string
}

type safetyScanPage struct {
	next string
	keys []string
}

func newSafetyRESPServer(t *testing.T) *safetyRESPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &safetyRESPServer{listener: listener}
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, conn := range s.connections {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			id := len(s.connections)
			s.connections = append(s.connections, conn)
			s.mu.Unlock()
			go s.serve(conn, id)
		}
	}()
	return s
}

func readSafetyRESP(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected RESP array")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(line, "$") {
			return nil, fmt.Errorf("expected bulk string")
		}
		length, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil {
			return nil, err
		}
		data := make([]byte, length+2)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:length])
	}
	return args, nil
}

func (s *safetyRESPServer) serve(conn net.Conn, id int) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	wrote := false
	for {
		args, err := readSafetyRESP(reader)
		if err != nil {
			return
		}
		name := strings.ToLower(args[0])
		s.mu.Lock()
		s.commands = append(s.commands, name)
		if ((name == "evalsha" || name == "eval") && !s.scriptNoop && s.scriptError == "") || name == "set" || name == "del" || name == "incr" {
			s.writes = append(s.writes, id)
			wrote = true
		}
		if name == "wait" {
			s.waits = append(s.waits, id)
		}
		s.mu.Unlock()
		response := "+OK\r\n"
		switch name {
		case "hello":
			response = "-ERR unknown command 'hello'\r\n"
		case "ping":
			response = "+PONG\r\n"
		case "evalsha", "eval":
			response = ":1\r\n"
			if s.onScript != nil {
				s.onScript()
			}
			if s.scriptError != "" {
				response = "-" + s.scriptError + "\r\n"
			}
			if s.closeScript {
				return
			}
		case "del", "incr":
			response = ":1\r\n"
		case "command":
			response = "*0\r\n"
		case "sentinel":
			response = "*0\r\n"
			if args[1] == "get-master-addr-by-name" {
				host, port, _ := net.SplitHostPort(s.sentinelMaster)
				response = fmt.Sprintf("*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(host), host, len(port), port)
			}
		case "subscribe":
			response = ""
			for index, channel := range args[1:] {
				response += fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(channel), channel, index+1)
			}
		case "scan":
			page := s.scanPages[args[1]]
			response = fmt.Sprintf("*2\r\n$%d\r\n%s\r\n*%d\r\n", len(page.next), page.next, len(page.keys))
			for _, key := range page.keys {
				response += fmt.Sprintf("$%d\r\n%s\r\n", len(key), key)
			}
		case "wait":
			if s.onWait != nil {
				s.onWait()
			}
			if s.closeWait {
				return
			}
			response = ":0\r\n"
			if wrote {
				response = ":1\r\n"
			}
		}
		if _, err := io.WriteString(conn, response); err != nil {
			return
		}
	}
}

func TestSafetyBarrierPreservesRedisHashSlots(t *testing.T) {
	for _, tc := range []struct {
		key  string
		slot uint16
	}{
		{"123456789", 12739}, {"{}foo", 9500}, {"foo{}", 5542}, {"foo{}{bar}", 8363}, {"{foo}:owner", 12182}, {"{bar}:lease", 5061},
	} {
		require.Equal(t, tc.slot, redisSafetySlot(tc.key), "reference vectors from go-redis hashtag tests")
		barrier, err := sameSlotSafetyBarrierKey(tc.key)
		require.NoError(t, err)
		require.NotEqual(t, tc.key, barrier)
		require.Equal(t, tc.slot, redisSafetySlot(barrier))
	}
}

func TestSafetyStandaloneAndSentinelTransportErrorsRemainUnconfirmed(t *testing.T) {
	for _, mode := range []string{"standalone", "sentinel", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			master := newSafetyRESPServer(t)
			master.closeScript = true
			var client *redislib.Client
			if mode == "sentinel" {
				sentinel := newSafetyRESPServer(t)
				sentinel.sentinelMaster = master.listener.Addr().String()
				client = redislib.NewFailoverClient(&redislib.FailoverOptions{MasterName: "sandbox-test", SentinelAddrs: []string{sentinel.listener.Addr().String()}, Protocol: 2, DisableIdentity: true, MaxRetries: -1})
			} else {
				client = redislib.NewClient(&redislib.Options{Addr: master.listener.Addr().String(), Protocol: 2, DisableIdentity: true, MaxRetries: -1, ContextTimeoutEnabled: true})
			}
			t.Cleanup(func() { _ = client.Close() })
			ctx := context.Background()
			if mode == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
			_, err := store.CompareAndDeleteIfAbsent(ctx, "{foo}:owner", []byte("old"), "{foo}:lease")
			require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed, "a primary read-back cannot turn a transport failure into replica evidence")
			master.mu.Lock()
			defer master.mu.Unlock()
			require.Empty(t, master.waits)
		})
	}
}

func TestSafetyWaitTransportFailureNeverReplaysWriteAndRefreshesCluster(t *testing.T) {
	old, current := newSafetyRESPServer(t), newSafetyRESPServer(t)
	old.closeWait = true
	var changed atomic.Bool
	old.onWait = func() { changed.Store(true) }
	client := redislib.NewClusterClient(&redislib.ClusterOptions{Protocol: 2, DisableIdentity: true,
		ClusterSlots: func(context.Context) ([]redislib.ClusterSlot, error) {
			address := old.listener.Addr().String()
			if changed.Load() {
				address = current.listener.Addr().String()
			}
			return []redislib.ClusterSlot{{Start: 0, End: 16383, Nodes: []redislib.ClusterNode{{Addr: address}}}}, nil
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
	_, err := store.CompareAndDeleteIfAbsent(context.Background(), "{foo}:owner", []byte("old"), "{foo}:lease")
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	old.mu.Lock()
	require.Len(t, old.writes, 2, "one script and one barrier, never replayed after failed WAIT")
	old.mu.Unlock()
	require.Eventually(t, func() bool {
		_, err := store.CompareAndDeleteIfAbsent(context.Background(), "{foo}:owner", []byte("old"), "{foo}:lease")
		return err == nil
	}, time.Second, time.Millisecond)
}

type stealSafetyConnectionHook struct {
	client *redislib.Client
	held   *redislib.Conn
}

func (h *stealSafetyConnectionHook) DialHook(next redislib.DialHook) redislib.DialHook { return next }
func (h *stealSafetyConnectionHook) ProcessPipelineHook(next redislib.ProcessPipelineHook) redislib.ProcessPipelineHook {
	return next
}
func (h *stealSafetyConnectionHook) ProcessHook(next redislib.ProcessHook) redislib.ProcessHook {
	return func(ctx context.Context, cmd redislib.Cmder) error {
		err := next(ctx, cmd)
		if h.held == nil && err == nil && (cmd.Name() == "evalsha" || cmd.Name() == "eval" || cmd.Name() == "set" || cmd.Name() == "del" || cmd.Name() == "incr") {
			// Occupy the pooled writer connection before the old independent WAIT.
			// A correctly pinned script keeps its connection outside the parent pool.
			h.held = h.client.Conn()
			if pingErr := h.held.Ping(ctx).Err(); pingErr != nil {
				return pingErr
			}
		}
		return err
	}
}

func TestEveryAtomicStoreWritePinsItsReplicaAck(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Store) error
	}{
		{"set", func(s *Store) error { return s.Set(context.Background(), "{owner}:owner", []byte("next"), 0) }},
		{"setnx", func(s *Store) error {
			_, err := s.SetNX(context.Background(), "{owner}:owner", []byte("next"), time.Second)
			return err
		}},
		{"delete", func(s *Store) error { return s.Delete(context.Background(), "{owner}:owner") }},
		{"increment", func(s *Store) error { _, err := s.Increment(context.Background(), "{owner}:generation"); return err }},
		{"cas", func(s *Store) error {
			_, err := s.CompareAndSwap(context.Background(), "{owner}:owner", []byte("old"), []byte("next"), 0)
			return err
		}},
		{"comparedelete", func(s *Store) error {
			_, err := s.CompareAndDelete(context.Background(), "{owner}:owner", []byte("old"))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSafetyRESPServer(t)
			client := redislib.NewClient(&redislib.Options{Addr: server.listener.Addr().String(), Protocol: 2, DisableIdentity: true, PoolSize: 8})
			t.Cleanup(func() { _ = client.Close() })
			hook := &stealSafetyConnectionHook{client: client}
			client.AddHook(hook)
			t.Cleanup(func() {
				if hook.held != nil {
					_ = hook.held.Close()
				}
			})
			store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
			require.NoError(t, tc.run(store))
			server.mu.Lock()
			defer server.mu.Unlock()
			require.Len(t, server.writes, 2, "state write plus real replication barrier")
			require.Len(t, server.waits, 1)
			require.Equal(t, server.waits[0], server.writes[0])
			require.Equal(t, server.waits[0], server.writes[1])
		})
	}
}

func TestSafetyScriptKeepsDefaultModesOnFastPath(t *testing.T) {
	for _, mode := range []DurabilityMode{DurabilityBestEffort, DurabilityNative} {
		t.Run(string(mode), func(t *testing.T) {
			server := newSafetyRESPServer(t)
			client := redislib.NewClient(&redislib.Options{Addr: server.listener.Addr().String(), Protocol: 2, DisableIdentity: true, PoolSize: 8})
			t.Cleanup(func() { _ = client.Close() })
			store := &Store{client: client, durability: mode}
			deleted, err := store.CompareAndDeleteIfAbsent(context.Background(), "{owner}:owner", []byte("old"), "{owner}:lease")
			require.NoError(t, err)
			require.True(t, deleted)
			server.mu.Lock()
			defer server.mu.Unlock()
			require.Empty(t, server.waits)
			require.Len(t, server.connections, 1)
		})
	}
}

func TestSafetyWriteAndWaitUseSamePooledConnection(t *testing.T) {
	server := newSafetyRESPServer(t)
	client := redislib.NewClient(&redislib.Options{Addr: server.listener.Addr().String(), Protocol: 2, DisableIdentity: true, PoolSize: 8})
	t.Cleanup(func() { _ = client.Close() })
	hook := &stealSafetyConnectionHook{client: client}
	client.AddHook(hook)
	t.Cleanup(func() {
		if hook.held != nil {
			_ = hook.held.Close()
		}
	})
	store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
	deleted, err := store.CompareAndDeleteIfAbsent(context.Background(), "{owner}:owner", []byte("old"), "{owner}:lease")
	require.NoError(t, err, "WAIT must acknowledge the script's own write offset")
	require.True(t, deleted)
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.waits, 1)
	for _, id := range server.writes {
		require.Equal(t, server.waits[0], id)
	}
}

func TestSafetyNoopScriptUsesRealSameConnectionBarrier(t *testing.T) {
	server := newSafetyRESPServer(t)
	server.scriptNoop = true
	client := redislib.NewClient(&redislib.Options{Addr: server.listener.Addr().String(), Protocol: 2, DisableIdentity: true, PoolSize: 8})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
	cmd, durabilityErr := store.runSafetyScript(context.Background(), redislib.NewScript("return 1"), []string{"{owner}:owner"})
	result, err := cmd.Int64()
	require.NoError(t, err)
	require.EqualValues(t, 1, result)
	require.NoError(t, durabilityErr, "a no-op must establish a current-master replication barrier before WAIT")
	server.mu.Lock()
	defer server.mu.Unlock()
	require.Len(t, server.writes, 1, "only the barrier should write")
	require.Equal(t, server.writes, server.waits)
}

func TestSafetyClusterRedirectReloadsBeforeNextAttempt(t *testing.T) {
	for _, redirect := range []string{"MOVED", "ASK", "READONLY"} {
		t.Run(redirect, func(t *testing.T) {
			old, current := newSafetyRESPServer(t), newSafetyRESPServer(t)
			old.scriptError = redirect + " 12182 " + current.listener.Addr().String()
			if redirect == "READONLY" {
				old.scriptError = "READONLY You can't write against a read only replica"
			}
			var changed atomic.Bool
			old.onScript = func() { changed.Store(true) }
			client := redislib.NewClusterClient(&redislib.ClusterOptions{Protocol: 2, DisableIdentity: true,
				ClusterSlots: func(context.Context) ([]redislib.ClusterSlot, error) {
					address := old.listener.Addr().String()
					if changed.Load() {
						address = current.listener.Addr().String()
					}
					return []redislib.ClusterSlot{{Start: 0, End: 16383, Nodes: []redislib.ClusterNode{{Addr: address}}}}, nil
				},
			})
			t.Cleanup(func() { _ = client.Close() })
			store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
			_, err := store.CompareAndDeleteIfAbsent(context.Background(), "{foo}:owner", []byte("old"), "{foo}:lease")
			require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed, "a redirect is pending, never an authorized write")
			require.Eventually(t, func() bool {
				_, err := store.CompareAndDeleteIfAbsent(context.Background(), "{foo}:owner", []byte("old"), "{foo}:lease")
				return err == nil
			}, time.Second, time.Millisecond)
			old.mu.Lock()
			require.Empty(t, old.waits, "no WAIT or barrier may run after failed Lua")
			old.mu.Unlock()
		})
	}
}

func TestClusterKeysScansEveryMasterAndEveryCursor(t *testing.T) {
	low, high := newSafetyRESPServer(t), newSafetyRESPServer(t)
	low.scanPages = map[string]safetyScanPage{"0": {"17", []string{"owner-a"}}, "17": {"0", []string{"owner-shared"}}}
	high.scanPages = map[string]safetyScanPage{"0": {"23", []string{"owner-b"}}, "23": {"0", []string{"owner-shared"}}}
	client := redislib.NewClusterClient(&redislib.ClusterOptions{Protocol: 2, DisableIdentity: true,
		ClusterSlots: func(context.Context) ([]redislib.ClusterSlot, error) {
			return []redislib.ClusterSlot{
				{Start: 0, End: 8191, Nodes: []redislib.ClusterNode{{Addr: low.listener.Addr().String()}}},
				{Start: 8192, End: 16383, Nodes: []redislib.ClusterNode{{Addr: high.listener.Addr().String()}}},
			}, nil
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	store := &Store{client: client}
	keys, err := store.Keys(context.Background(), "owner-*")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"owner-a", "owner-b", "owner-shared"}, keys)
}

func TestSafetyWriteAndWaitUseKeyMasterInCluster(t *testing.T) {
	low, high := newSafetyRESPServer(t), newSafetyRESPServer(t)
	client := redislib.NewClusterClient(&redislib.ClusterOptions{Protocol: 2, DisableIdentity: true, PoolSize: 8,
		ClusterSlots: func(context.Context) ([]redislib.ClusterSlot, error) {
			return []redislib.ClusterSlot{
				{Start: 0, End: 8191, Nodes: []redislib.ClusterNode{{Addr: low.listener.Addr().String()}}},
				{Start: 8192, End: 16383, Nodes: []redislib.ClusterNode{{Addr: high.listener.Addr().String()}}},
			}, nil
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	master, err := client.MasterForKey(context.Background(), "{foo}:owner")
	require.NoError(t, err)
	_, expectedPort, err := net.SplitHostPort(high.listener.Addr().String())
	require.NoError(t, err)
	_, actualPort, err := net.SplitHostPort(master.Options().Addr)
	require.NoError(t, err)
	require.Equal(t, expectedPort, actualPort)
	hook := &stealSafetyConnectionHook{client: master}
	master.AddHook(hook)
	t.Cleanup(func() {
		if hook.held != nil {
			_ = hook.held.Close()
		}
	})
	store := &Store{client: client, durability: DurabilityReplicaAck, ackReplicas: 1, ackTimeout: time.Millisecond}
	deleted, err := store.CompareAndDeleteIfAbsent(context.Background(), "{foo}:owner", []byte("old"), "{foo}:lease")
	require.NoError(t, err)
	require.True(t, deleted)
	low.mu.Lock()
	require.Empty(t, low.writes)
	require.Empty(t, low.waits, "WAIT must not select an unrelated master")
	low.mu.Unlock()
	high.mu.Lock()
	defer high.mu.Unlock()
	require.Len(t, high.waits, 1)
	for _, id := range high.writes {
		require.Equal(t, high.waits[0], id)
	}
}

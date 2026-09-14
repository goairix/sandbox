package redisbootstrap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLocalObserverInventoryReadsActualPVCWithoutRedis(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	provider, err := NewLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: "12345678901234567890123456789012"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := provider(context.Background(), InventoryProof)
	if err != nil || observation.Volume.Identity == nil || observation.Snapshot == nil || observation.ConfigDigest == "" || observation.RunID != "" {
		t.Fatalf("inventory must read configured PVC without Redis: %+v, %v", observation, err)
	}
}

func TestLocalObserverRejectsInvalidConstruction(t *testing.T) {
	options := LocalObserverOptions{Directory: "/trusted/pvc/not-required-to-exist", Cluster: testCluster(), Member: testMember(testCluster(), 0), MasterName: "main", Password: strings.Repeat("a", 32)}
	if _, err := NewLocalObserver(options); err != nil {
		t.Fatalf("constructor must not read PVC: %v", err)
	}
	for _, name := range []string{"relative", "root", "empty path", "bad member", "bad cluster", "empty master", "long master", "unicode master", "space master", "short password", "rewrite password"} {
		t.Run(name, func(t *testing.T) {
			o := options
			switch name {
			case "relative":
				o.Directory = "relative"
			case "root":
				o.Directory = "/some/.."
			case "empty path":
				o.Directory = ""
			case "bad member":
				o.Member.Ordinal = 4
			case "bad cluster":
				o.Cluster.Phase = "invalid"
			case "empty master":
				o.MasterName = ""
			case "long master":
				o.MasterName = strings.Repeat("a", 129)
			case "unicode master":
				o.MasterName = "māster"
			case "space master":
				o.MasterName = "a b"
			case "short password":
				o.Password = "redact_me"
			case "rewrite password":
				o.Password += "\""
			}
			provider, err := NewLocalObserver(o)
			if err == nil || provider != nil || strings.Contains(err.Error(), o.Password) {
				t.Fatalf("invalid constructor accepted or leaked password: %v", err)
			}
		})
	}
}

func TestLocalObserverLiveUsesAuthenticatedActualRESP(t *testing.T) {
	const password = "12345678901234567890123456789012"
	runID := strings.Repeat("a", 40)
	address := localObserverRESPServer(t, password, "# Server\r\nredis_version:7.4.0\r\nrun_id:"+runID+"\r\n", false)
	dir, c, m := configuredLocalFixture(t)
	provider, err := newLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: password}, ReadLocalVolume,
		func(ctx context.Context, password string) (string, error) {
			return readLocalRedisRunID(ctx, address, password)
		})
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider(context.Background(), LiveProof)
	if err != nil || got.RunID != runID || got.Snapshot == nil || got.Volume.Identity == nil {
		t.Fatalf("actual AUTH/INFO: %+v, %v", got, err)
	}
	_, err = readLocalRedisRunID(context.Background(), address, strings.Repeat("b", 32))
	if err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "WRONGPASS") {
		t.Fatalf("wrong auth must fail redacted: %v", err)
	}
}

func TestLocalObserverFailedDialIsOneShot(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	id, err := readLocalRedisRunIDWithDialer(context.Background(), address, strings.Repeat("a", 32), func(ctx context.Context, network, address string) (net.Conn, error) {
		attempts.Add(1)
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
	})
	if err == nil || id != "" {
		t.Fatal("actual refused TCP dial must not confirm a process")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("one local observation must issue one actual dial, got %d", got)
	}
	timer := time.NewTimer(150 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if got := attempts.Load(); got != 1 {
		t.Fatalf("closed observer issued detached actual dial: %d", got)
	}
}

func TestLocalObserverINFORejectsMalformed(t *testing.T) {
	valid := "# Server\r\nrun_id:" + strings.Repeat("a", 40) + "\r\n"
	for _, info := range []string{"", "redis_version:7\r\n", valid + valid, "run_id:" + strings.Repeat("A", 40) + "\r\n", "run_id:" + strings.Repeat("a", 39) + "\r\n", valid + "other:\xff\r\n", valid + "other:\t\r\n", strings.Repeat("x", 65537)} {
		id, err := parseLocalRunID(info)
		if err == nil || id != "" {
			t.Errorf("invalid INFO accepted")
		}
	}
	id, err := parseLocalRunID(valid)
	if err != nil || id != strings.Repeat("a", 40) {
		t.Fatalf("valid INFO rejected: %v", err)
	}
}

func TestLocalObserverRejectsUnstablePVCAndDependencyErrors(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	volume, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"identity", "snapshot", "digest", "second read", "first read", "info", "cancel", "reserved", "purpose"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider, err := newLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: strings.Repeat("a", 32)},
				func(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error) {
					reads++
					copy := volume
					id := *volume.Volume.Identity
					state := *volume.Volume.Persisted
					snapshot := *volume.Snapshot
					copy.Volume.Identity = &id
					copy.Volume.Persisted = &state
					copy.Snapshot = &snapshot
					if name == "first read" || (name == "second read" && reads == 2) {
						return LocalVolumeSnapshot{}, errors.New("secret dependency error")
					}
					if name == "reserved" {
						copy.Volume.Identity.InitialConfig = Reserved
						copy.Snapshot = nil
						copy.Volume.Persisted = nil
						copy.ConfigDigest = ""
					}
					if reads == 2 {
						switch name {
						case "identity":
							copy.Volume.Identity.MarkerID = strings.Repeat("b", 32)
						case "snapshot":
							copy.Snapshot.CurrentEpoch++
						case "digest":
							copy.ConfigDigest = strings.Repeat("b", 64)
						}
					}
					return copy, nil
				}, func(context.Context, string) (string, error) {
					if name == "info" {
						return "", errors.New("secret dependency error")
					}
					if name == "cancel" {
						cancel()
					}
					return strings.Repeat("a", 40), nil
				})
			if err != nil {
				t.Fatal(err)
			}
			purpose := LiveProof
			if name == "purpose" {
				purpose = "bad"
			}
			got, err := provider(ctx, purpose)
			if err == nil || !reflect.DeepEqual(got, LocalObservation{}) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe observation returned: %+v,%v", got, err)
			}
			if name == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
		})
	}
}

func TestLocalObserverActualClientTimeoutAndOversize(t *testing.T) {
	password := strings.Repeat("a", 32)
	address := localObserverRESPServer(t, password, "", true)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if id, err := readLocalRedisRunID(ctx, address, password); err == nil || id != "" {
		t.Fatal("stalled client accepted")
	}
	if time.Since(start) > 700*time.Millisecond {
		t.Fatal("context timeout ignored")
	}
	address = localObserverRESPServer(t, password, strings.Repeat("x", 65537), false)
	if id, err := readLocalRedisRunID(context.Background(), address, password); err == nil || id != "" {
		t.Fatal("oversize response accepted")
	}
}

func TestLocalObserverClientCloseFailureIsNotSuccess(t *testing.T) {
	password := strings.Repeat("a", 32)
	address := localObserverRESPServer(t, password, "run_id:"+strings.Repeat("a", 40)+"\r\n", false)
	closed := make(chan struct{}, 1)
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &localObserverCloseFailureConn{Conn: connection, closed: closed}, nil
	}
	id, err := readLocalRedisRunIDWithDialer(context.Background(), address, password, dial)
	if err == nil || id != "" {
		t.Fatal("close failure returned successful live identity")
	}
	select {
	case <-closed:
	default:
		t.Fatal("client did not close actual TCP connection")
	}
}

type localObserverCloseFailureConn struct {
	net.Conn
	closed chan struct{}
}

func (c *localObserverCloseFailureConn) Close() error {
	err := c.Conn.Close()
	select {
	case c.closed <- struct{}{}:
	default:
	}
	return errors.Join(err, errors.New("private close detail"))
}

func TestLocalObserverBoundedRESPFrames(t *testing.T) {
	for _, raw := range []string{"$2147483647\r\n", "$65537\r\n", "*65\r\n", "*-2\r\n", "$x\r\n", "$0\r\nx\n", "+bad\n", "+" + strings.Repeat("a", 1024) + "\r\n", "%0\r\n", strings.Repeat("*1\r\n", 10) + "+OK\r\n"} {
		if err := readLocalRESPFrame(bufio.NewReaderSize(strings.NewReader(raw), 1024), &bytes.Buffer{}, 0); err == nil {
			t.Error("unbounded/invalid RESP accepted")
		}
	}
	for _, raw := range []string{"+OK\r\n", "-ERR test\r\n", ":1\r\n", "$-1\r\n", "*-1\r\n", "*2\r\n$3\r\nkey\r\n:1\r\n", fmt.Sprintf("$%d\r\n%s\r\n", 65536, strings.Repeat("a", 65536))} {
		frame := &bytes.Buffer{}
		if err := readLocalRESPFrame(bufio.NewReaderSize(strings.NewReader(raw), 1024), frame, 0); err != nil || frame.String() != raw {
			t.Fatalf("valid RESP rejected: %v", err)
		}
	}
}

func TestLocalObserverInventoryNeverReadsProcessForAnyVolumeState(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	configured, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	reserved := LocalVolumeSnapshot{Volume: VolumeState{Identity: &VolumeIdentity{ClusterID: c.ClusterID, Member: m, MarkerID: configured.Volume.Identity.MarkerID, InitialConfig: Reserved}}}
	for _, volume := range []LocalVolumeSnapshot{{Volume: VolumeState{Empty: true}}, reserved, configured} {
		reads := 0
		provider, err := newLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: strings.Repeat("a", 32)}, func(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error) {
			reads++
			return volume, nil
		}, func(context.Context, string) (string, error) {
			t.Fatal("inventory attempted Redis connection")
			return "", errors.New("unexpected")
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := provider(context.Background(), InventoryProof)
		want := LocalObservation{Volume: volume.Volume, Snapshot: volume.Snapshot, ConfigDigest: volume.ConfigDigest}
		if err != nil || !reflect.DeepEqual(got, want) || reads != 1 {
			t.Fatalf("inventory changed volume state: %+v,%v", got, err)
		}
	}
}

func TestLocalObserverCancellationAndMalformedEvidence(t *testing.T) {
	dir, c, m := configuredLocalFixture(t)
	volume, err := ReadLocalVolume(context.Background(), dir, c, m, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pre-cancel", "inventory cancel", "inventory invalid", "bad run id", "second read cancel", "dial failure", "INFO malformed"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "pre-cancel" {
				cancel()
			}
			reads := 0
			provider, err := newLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: strings.Repeat("a", 32)}, func(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error) {
				reads++
				copy := volume
				if name == "inventory invalid" {
					copy.ConfigDigest = "invalid"
				}
				if name == "inventory cancel" || (name == "second read cancel" && reads == 2) {
					cancel()
				}
				return copy, nil
			}, func(context.Context, string) (string, error) { return "bad-run-id", nil })
			if err != nil {
				t.Fatal(err)
			}
			purpose := LiveProof
			if strings.HasPrefix(name, "inventory") {
				purpose = InventoryProof
			}
			got, err := provider(ctx, purpose)
			if err == nil || !reflect.DeepEqual(got, LocalObservation{}) {
				t.Fatal("invalid evidence accepted")
			}
			if name == "pre-cancel" && reads != 0 {
				t.Fatal("canceled provider read PVC")
			}
			if name == "dial failure" {
				id, err := readLocalRedisRunIDWithDialer(context.Background(), "127.0.0.1:1", strings.Repeat("a", 32), func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("secret dial error") })
				if err == nil || id != "" || strings.Contains(err.Error(), "secret") {
					t.Fatal("dial failure leaked")
				}
			}
			if name == "INFO malformed" {
				address := localObserverRESPServer(t, strings.Repeat("a", 32), "run_id:bad\r\n", false)
				if id, err := readLocalRedisRunID(context.Background(), address, strings.Repeat("a", 32)); err == nil || id != "" {
					t.Fatal("malformed real response accepted")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := readLocalRedisRunID(ctx, "127.0.0.1:1", strings.Repeat("a", 32)); id != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled actual reader returned identity")
	}
	for _, deps := range []struct {
		read func(context.Context, string, ClusterState, Member, string) (LocalVolumeSnapshot, error)
		run  func(context.Context, string) (string, error)
	}{{nil, func(context.Context, string) (string, error) { return "", nil }}, {ReadLocalVolume, nil}} {
		if provider, err := newLocalObserver(LocalObserverOptions{Directory: dir, Cluster: c, Member: m, MasterName: "main", Password: strings.Repeat("a", 32)}, deps.read, deps.run); provider != nil || err == nil {
			t.Fatal("missing dependency accepted")
		}
	}
}

// This server exercises go-redis's actual handshake/AUTH/INFO over local TCP.
func localObserverRESPServer(t *testing.T, password, info string, stall bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	stop := make(chan struct{})
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() {
					if err := connection.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
						t.Errorf("server close: %v", err)
					}
				}()
				if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Errorf("deadline: %v", err)
					return
				}
				reader := bufio.NewReader(connection)
				authenticated := false
				for {
					args, err := readLocalObserverTestCommand(reader)
					if err != nil {
						return
					}
					if stall {
						<-stop
						return
					}
					response := "+OK\r\n"
					switch strings.ToLower(args[0]) {
					case "hello":
						response = "-ERR unknown command 'hello'\r\n"
					case "auth":
						if len(args) == 2 && args[1] == password {
							authenticated = true
						} else {
							response = "-WRONGPASS invalid password\r\n"
						}
					case "info":
						if !authenticated {
							response = "-NOAUTH Authentication required\r\n"
						} else if len(args) != 2 || args[1] != "server" {
							t.Error("client did not request INFO server")
							return
						} else {
							response = fmt.Sprintf("$%d\r\n%s\r\n", len(info), info)
						}
					default:
						t.Errorf("unexpected command %q", args[0])
						return
					}
					if _, err := io.WriteString(connection, response); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
		workers.Wait()
	})
	return listener.Addr().String()
}

func readLocalObserverTestCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("expected array")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || n <= 0 || n > 16 {
		return nil, fmt.Errorf("invalid array")
	}
	args := make([]string, n)
	for i := range args {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "$") {
			return nil, fmt.Errorf("invalid bulk")
		}
		size, err := strconv.Atoi(strings.TrimSpace(line[1:]))
		if err != nil || size < 0 || size > 1024 {
			return nil, fmt.Errorf("invalid length")
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		args[i] = string(data[:size])
	}
	return args, nil
}

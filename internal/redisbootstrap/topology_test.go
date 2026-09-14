package redisbootstrap

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These are real TCP RESP2 and HTTP signed-proof fixtures, not Redis processes.
// The cache-only three-member integration is separate HA evidence.
type topologyFixture struct {
	registrationFixture
	options       TopologyOptions
	dependencies  topologyDependencies
	observations  [3]LocalObservation
	addresses     map[string]string
	urls          [3]string
	primary       int
	ack           int
	mutate        func(int, bool, []string, string) string
	disconnect    func(int, bool, []string) bool
	barriers      atomic.Int64
	waits         atomic.Int64
	proofRequests atomic.Int64
}

func newTopologyFixture(t *testing.T) *topologyFixture {
	t.Helper()
	f := &topologyFixture{registrationFixture: newRegistrationFixture(t), addresses: map[string]string{}, ack: 1}
	f.options = TopologyOptions{Registration: fixtureRegistration(f.registrationFixture), PublicKeys: f.keys, MasterName: "sandbox", DataPassword: strings.Repeat("d", 32), SentinelPassword: strings.Repeat("s", 32), AckTimeout: time.Second}
	for i := range 3 {
		o := configuredRegistrationObservation(f.registrationFixture, i, LiveProof)
		o.RunID = fmt.Sprintf("%040x", i+1)
		sum := sha256.Sum256(f.keys[i])
		o.Snapshot.SentinelID = hex.EncodeToString(sum[:20])
		f.observations[i] = o
		h, err := NewIdentityHandler(f.cluster, testMember(f.cluster, i), strings.Repeat("e", 64), f.private[i], func(_ context.Context, purpose ProofPurpose) (LocalObservation, error) {
			f.proofRequests.Add(1)
			o := f.observations[i]
			if purpose == InventoryProof {
				o.RunID = ""
			}
			return o, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		f.urls[i] = s.URL + "/v1/identity"
		for _, sentinel := range []bool{false, true} {
			port := "6379"
			if sentinel {
				port = "26379"
			}
			f.addresses[f.cluster.Members[i]+":"+port] = f.listen(t, i, sentinel)
		}
	}
	f.dependencies = topologyDependencies{
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			a, ok := f.addresses[address]
			if !ok {
				return nil, errors.New("invalid fixed endpoint")
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, a)
		},
		fetch: func(ctx context.Context, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, current *AuthenticatedEndpoint) (IdentityProof, IdentityContactState, error) {
			return fetchIdentityProofContact(ctx, f.urls[m.Ordinal], c, m, key, ch, expected, current)
		},
	}
	return f
}

func (f *topologyFixture) listen(t *testing.T, member int, sentinel bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[c] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = c.Close(); mu.Lock(); delete(connections, c); mu.Unlock() }()
				reader := bufio.NewReader(c)
				authed, wrote := false, false
				for {
					args, err := readLocalObserverTestCommand(reader)
					if err != nil {
						return
					}
					command := strings.ToUpper(args[0])
					reply := "-ERR unknown command\r\n"
					switch {
					case command == "HELLO": // Match real Redis fallback to AUTH.
					case command == "AUTH":
						password := f.options.DataPassword
						if sentinel {
							password = f.options.SentinelPassword
						}
						if len(args) == 2 && args[1] == password {
							authed = true
							reply = "+OK\r\n"
						} else {
							reply = "-WRONGPASS PRIVATE-FIXTURE-CREDENTIAL\r\n"
						}
					case !authed:
						reply = "-NOAUTH Authentication required\r\n"
					case command == "CLIENT":
						reply = "+OK\r\n"
					case command == "INFO" && !sentinel:
						reply = topologyBulk(f.info(member))
					case command == "CONFIG" && len(args) == 3 && args[1] == "get" && !sentinel:
						value := map[string]string{"appendonly": "yes", "appendfsync": "everysec", "min-replicas-to-write": "1", "min-replicas-max-lag": "10"}[args[2]]
						reply = topologyArray(args[2], value)
					case command == "SENTINEL" && sentinel && len(args) >= 2:
						switch strings.ToUpper(args[1]) {
						case "MYID":
							reply = topologyBulk(f.observations[member].Snapshot.SentinelID)
						case "MASTER":
							reply = f.masterReply(member)
						case "SENTINELS":
							reply = "*2\r\n"
							for i := range 3 {
								if i != member {
									reply += topologyArray("name", f.observations[i].Snapshot.SentinelID, "ip", f.cluster.Members[i], "port", "26379", "runid", f.observations[i].Snapshot.SentinelID, "flags", "sentinel", "last-hello-message", "10")
								}
							}
						case "CKQUORUM":
							reply = "+OK 3 usable Sentinels. Quorum and failover authorization can be reached\r\n"
						}
					case command == "SET" && !sentinel:
						if member == f.primary && len(args) == 5 && strings.HasPrefix(args[1], "sandbox:bootstrap-barrier:") && lowerHex(strings.TrimPrefix(args[1], "sandbox:bootstrap-barrier:"), 32) && args[2] == "1" && strings.ToUpper(args[3]) == "PX" && args[4] == "60000" {
							wrote = true
							f.barriers.Add(1)
							reply = "+OK\r\n"
						}
					case command == "WAIT" && !sentinel:
						f.waits.Add(1)
						if wrote && len(args) == 3 && args[1] == "1" {
							reply = fmt.Sprintf(":%d\r\n", f.ack)
						} else {
							reply = ":0\r\n"
						}
					}
					if f.mutate != nil {
						reply = f.mutate(member, sentinel, args, reply)
					}
					if f.disconnect != nil && f.disconnect(member, sentinel, args) {
						return
					}
					if _, err := io.WriteString(c, reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-acceptDone
		mu.Lock()
		for c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return l.Addr().String()
}

func topologyBulk(value string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(value), value) }
func topologyArray(values ...string) string {
	out := fmt.Sprintf("*%d\r\n", len(values))
	for _, v := range values {
		out += topologyBulk(v)
	}
	return out
}

func (f *topologyFixture) info(member int) string {
	o := f.observations[member]
	info := "# Server\r\nrun_id:" + o.RunID + "\r\n# Replication\r\nmaster_replid:" + strings.Repeat("a", 40) + "\r\n"
	if member == f.primary {
		info += "role:master\r\nconnected_slaves:2\r\nmaster_repl_offset:0\r\n"
		n := 0
		for i := range 3 {
			if i != member {
				info += fmt.Sprintf("slave%d:ip=%s,port=6379,state=online,offset=0,lag=0\r\n", n, f.cluster.Members[i])
				n++
			}
		}
	} else {
		info += "role:slave\r\nmaster_host:" + f.cluster.Members[f.primary] + "\r\nmaster_port:6379\r\nmaster_link_status:up\r\nmaster_sync_in_progress:0\r\nslave_repl_offset:0\r\n"
	}
	return info
}

func (f *topologyFixture) masterReply(member int) string {
	// Mirrors normal Redis 7 Sentinel MASTER fields (20 pairs), including fields
	// we don't use. No credential/config raw payload belongs in diagnostics.
	return topologyArray("name", "sandbox", "ip", f.cluster.Members[f.primary], "port", "6379", "runid", f.observations[f.primary].RunID, "flags", "master", "link-pending-commands", "0", "link-refcount", "1", "last-ping-sent", "0", "last-ok-ping-reply", "10", "last-ping-reply", "10", "down-after-milliseconds", "5000", "info-refresh", "10", "role-reported", "master", "role-reported-time", "10", "config-epoch", strconv.FormatUint(f.observations[member].Snapshot.State.SentinelEpoch, 10), "num-slaves", "2", "num-other-sentinels", "2", "quorum", "2", "failover-timeout", "10000", "parallel-syncs", "1")
}

func TestTopologyActualRESPAndSignedProofRequiresWrittenBarrier(t *testing.T) {
	f := newTopologyFixture(t)
	if err := verifyTopology(context.Background(), f.options, true, f.dependencies); err != nil {
		t.Fatal("valid authenticated topology rejected:", err)
	}
	if f.barriers.Load() != 1 || f.waits.Load() != 1 || f.proofRequests.Load() != 6 {
		t.Fatalf("must independently verify 3 inventories, 3 current live proofs, then same-connection SET/WAIT: barriers=%d waits=%d proofs=%d", f.barriers.Load(), f.waits.Load(), f.proofRequests.Load())
	}
}

func TestTopologyACKInsufficientNeverOpensGate(t *testing.T) {
	f := newTopologyFixture(t)
	f.ack = 0
	if err := verifyTopology(context.Background(), f.options, true, f.dependencies); err == nil {
		t.Fatal("WAIT zero must not initialize")
	}
	if f.barriers.Load() != 1 || f.waits.Load() != 1 {
		t.Fatal("must test a real connection's new write before failed ACK")
	}
}

func TestTopologyRejectsActualEndpointAndStateFailures(t *testing.T) {
	for _, name := range []string{"data auth", "sentinel auth", "replica link", "replication lineage", "unsafe config", "Sentinel id", "Sentinel monitor", "Sentinel peer", "quorum", "signed run id", "higher minority", "second INFO replacement", "same process twice"} {
		t.Run(name, func(t *testing.T) {
			f := newTopologyFixture(t)
			var infoReads atomic.Int64
			f.mutate = func(member int, sentinel bool, args []string, reply string) string {
				command := strings.ToUpper(args[0])
				switch name {
				case "data auth":
					if command == "AUTH" && !sentinel {
						return "-WRONGPASS PRIVATE-FIXTURE-CREDENTIAL\r\n"
					}
				case "sentinel auth":
					if command == "AUTH" && sentinel {
						return "-WRONGPASS PRIVATE-FIXTURE-CREDENTIAL\r\n"
					}
				case "replica link":
					if command == "INFO" && member == 1 {
						return strings.Replace(reply, "master_link_status:up", "master_link_status:no", 1)
					}
				case "replication lineage":
					if command == "INFO" && member == 1 {
						return strings.Replace(reply, strings.Repeat("a", 40), strings.Repeat("b", 40), 1)
					}
				case "unsafe config":
					if command == "CONFIG" {
						return topologyArray(args[2], "no")
					}
				case "Sentinel id":
					if command == "SENTINEL" && strings.EqualFold(args[1], "MYID") {
						return topologyBulk(strings.Repeat("a", 40))
					}
				case "Sentinel monitor":
					if command == "SENTINEL" && strings.EqualFold(args[1], "MASTER") {
						return strings.Replace(reply, "master\r\n", "s_down\r\n", 1)
					}
				case "Sentinel peer":
					if command == "SENTINEL" && strings.EqualFold(args[1], "SENTINELS") {
						return "*0\r\n"
					}
				case "quorum":
					if command == "SENTINEL" && strings.EqualFold(args[1], "CKQUORUM") {
						return "-NOQUORUM PRIVATE-FIXTURE-CREDENTIAL\r\n"
					}
				case "signed run id":
					if command == "INFO" && member == 1 {
						return topologyBulk(strings.Replace(f.info(member), f.observations[member].RunID, strings.Repeat("e", 40), 1))
					}
				case "second INFO replacement":
					if command == "INFO" && member == 0 && infoReads.Add(1) > 1 {
						return topologyBulk(strings.Replace(f.info(member), f.observations[member].RunID, strings.Repeat("e", 40), 1))
					}
				}
				return reply
			}
			if name == "higher minority" {
				f.observations[2].Snapshot.State.SentinelEpoch = 4
				f.observations[2].Volume.Persisted.SentinelEpoch = 4
			}
			if name == "same process twice" {
				f.observations[1].RunID = f.observations[2].RunID
			}
			err := verifyTopology(context.Background(), f.options, true, f.dependencies)
			if err == nil || err.Error() != errTopology.Error() || strings.Contains(err.Error(), "PRIVATE-FIXTURE-CREDENTIAL") {
				t.Fatalf("invalid authenticated/signed topology accepted or detail leaked: %v", err)
			}
		})
	}
}

func TestTopologyInitializedAllowsOneUnreachableMember(t *testing.T) {
	f := newTopologyFixture(t)
	f.options.Registration.Cluster.Phase = Initialized
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f.urls[2] = "http://" + address + "/v1/identity"
	if err := verifyTopology(context.Background(), f.options, false, f.dependencies); err != nil {
		t.Fatal("initialized 2 healthy members plus actual ACK1 must pass:", err)
	}
	if f.barriers.Load() != 1 || f.waits.Load() != 1 {
		t.Fatal("one unavailable member must not waive ACK")
	}
}

func TestTopologyPendingDoesNotWaiveThirdMember(t *testing.T) {
	f := newTopologyFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	f.urls[2] = "http://" + address + "/v1/identity"
	if err := verifyTopology(context.Background(), f.options, false, f.dependencies); err == nil {
		t.Fatal("Pending must require all 3 regardless of flag")
	}
	if f.barriers.Load() != 0 {
		t.Fatal("unconfirmed fresh inventory must not write")
	}
}

func TestTopologyInitializedDoesNotIgnoreReachableRejectedIdentity(t *testing.T) {
	f := newTopologyFixture(t)
	f.options.Registration.Cluster.Phase = Initialized
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"identity request failed"}`)
	}))
	defer s.Close()
	f.urls[2] = s.URL + "/v1/identity"
	if err := verifyTopology(context.Background(), f.options, false, f.dependencies); err == nil {
		t.Fatal("reachable 503 identity must not be treated as offline")
	}
	if f.barriers.Load() != 0 {
		t.Fatal("unverified retained member must block before write")
	}
}

func TestTopologyFailedEndpointDialIsOneShot(t *testing.T) {
	for _, port := range []string{"6379", "26379"} {
		t.Run(port, func(t *testing.T) {
			f := newTopologyFixture(t)
			f.options.Registration.Cluster.Phase = Initialized
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := l.Addr().String()
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			endpoint := f.cluster.Members[2] + ":" + port
			f.addresses[endpoint] = address
			var failedDials atomic.Int64
			dial := f.dependencies.dial
			f.dependencies.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == endpoint {
					failedDials.Add(1)
				}
				return dial(ctx, network, address)
			}
			if err := verifyTopology(context.Background(), f.options, false, f.dependencies); err != nil {
				t.Fatal("one retained but unavailable process must permit 2 healthy plus ACK:", err)
			}
			if got := failedDials.Load(); got != 1 {
				t.Fatalf("each fixed process gets exactly one actual dial, got %d", got)
			}
			// All owned clients are now closed; retain a scheduler observation
			// window to catch detached pool probes. This isn't a retry wait.
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			<-timer.C
			if got := failedDials.Load(); got != 1 {
				t.Fatalf("closed attempt issued detached actual dial: %d", got)
			}
		})
	}
}

func TestTopologyEstablishedConnectionsNeverRedial(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(strconv.FormatBool(disconnect), func(t *testing.T) {
			f := newTopologyFixture(t)
			var mu sync.Mutex
			counts := map[string]int{}
			dial := f.dependencies.dial
			f.dependencies.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				mu.Lock()
				counts[address]++
				mu.Unlock()
				return dial(ctx, network, address)
			}
			var primaryInfos atomic.Int64
			if disconnect {
				f.disconnect = func(member int, sentinel bool, args []string) bool {
					return member == 0 && !sentinel && strings.EqualFold(args[0], "INFO") && primaryInfos.Add(1) > 1
				}
			}
			err := verifyTopology(context.Background(), f.options, true, f.dependencies)
			if (err != nil) != disconnect {
				t.Fatalf("established connection result incorrect: %v", err)
			}
			if f.barriers.Load() != 1 {
				t.Fatal("disconnect must occur only after actual new-write ACK")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(counts) != 6 {
				t.Fatal("must inspect all 6 actual authenticated TCP connections")
			}
			for _, count := range counts {
				if count != 1 {
					t.Fatal("success/established disconnect must never substitute a new socket")
				}
			}
		})
	}
}

func TestTopologyInvalidOptionsAndCancellationPerformNoIO(t *testing.T) {
	for _, name := range []string{"nil context", "cancelled", "key digest", "master name", "password", "same passwords", "short timeout", "long timeout"} {
		t.Run(name, func(t *testing.T) {
			f := newTopologyFixture(t)
			ctx := context.Background()
			switch name {
			case "nil context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "key digest":
				f.options.Registration.KeyDigest = strings.Repeat("a", 64)
			case "master name":
				f.options.MasterName = "PRIVATE-FIXTURE-CREDENTIAL\n"
			case "password":
				f.options.DataPassword = "PRIVATE-FIXTURE-CREDENTIAL"
			case "same passwords":
				f.options.SentinelPassword = f.options.DataPassword
			case "short timeout":
				f.options.AckTimeout = time.Millisecond
			case "long timeout":
				f.options.AckTimeout = 11 * time.Second
			}
			err := verifyTopology(ctx, f.options, true, f.dependencies)
			if err == nil || f.proofRequests.Load() != 0 || f.barriers.Load() != 0 {
				t.Fatal("invalid/cancelled input must fail without network/write")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost")
			}
		})
	}
}

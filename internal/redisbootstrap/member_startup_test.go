package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type startupFixture struct {
	f       registrationFixture
	r       BootstrapRegistration
	dirs    [3]string
	options MemberStartupOptions
}

func newStartupFixture(t *testing.T, ordinal int) startupFixture {
	t.Helper()
	f := newRegistrationFixture(t)
	r := fixtureRegistration(f)
	x := startupFixture{f: f, r: r}
	o := InitialConfigOptions{MasterName: "sandbox", DataPassword: strings.Repeat("d", 40), SentinelPassword: strings.Repeat("s", 40), DownAfterMilliseconds: 10000, FailoverTimeoutMilliseconds: 60000, ParallelSyncs: 1}
	for i := range x.dirs {
		x.dirs[i] = t.TempDir()
		if err := WriteVolumeIdentity(context.Background(), filepath.Join(x.dirs[i], "identity.json"), *f.proofs[i].Observation.Volume.Identity, f.cluster); err != nil {
			t.Fatal(err)
		}
		if _, err := ConfigureInitialLocalVolume(context.Background(), x.dirs[i], r, f.keys, testMember(f.cluster, i), o); err != nil {
			t.Fatal(err)
		}
	}
	control := t.TempDir()
	x.options = MemberStartupOptions{Directory: x.dirs[ordinal], Ordinal: ordinal, Initial: o, Timeout: 1100 * time.Millisecond, Files: BootstrapFilePaths{Cluster: filepath.Join(control, "cluster.json"), Registration: filepath.Join(control, "registration.json"), PublicKeys: filepath.Join(control, "keys.json")}}
	x.writeControls(t)
	return x
}

func (x startupFixture) writeControls(t *testing.T) {
	t.Helper()
	var encoded [3]string
	for i, k := range x.f.keys {
		encoded[i] = hex.EncodeToString(k)
	}
	keys, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for p, v := range map[string]any{x.options.Files.Cluster: x.r.Cluster, x.options.Files.Registration: x.r} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(x.options.Files.PublicKeys, keys, 0600); err != nil {
		t.Fatal(err)
	}
}

func (x startupFixture) rewrite(t *testing.T, i int, primary int, epoch uint64, redisRole Role) {
	t.Helper()
	rp := filepath.Join(x.dirs[i], "redis.conf")
	b, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	filtered := []string{}
	for _, l := range lines {
		if !strings.HasPrefix(l, "replicaof ") {
			filtered = append(filtered, l)
		}
	}
	if redisRole == Replica {
		filtered = append(filtered, "replicaof "+x.r.Cluster.Members[primary]+" 6379")
	}
	if err := os.WriteFile(rp, []byte(strings.Join(filtered, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(x.dirs[i], "sentinel.conf")
	b, err = os.ReadFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(string(b), "\n")
	for j, l := range lines {
		if strings.HasPrefix(l, "sentinel monitor ") {
			lines[j] = "sentinel monitor sandbox " + x.r.Cluster.Members[primary] + " 6379 2"
		}
		if strings.HasPrefix(l, "sentinel config-epoch ") {
			lines[j] = "sentinel config-epoch sandbox " + strconv.FormatUint(epoch, 10)
		}
		if strings.HasPrefix(l, "sentinel current-epoch ") {
			lines[j] = "sentinel current-epoch " + strconv.FormatUint(epoch, 10)
		}
	}
	if err := os.WriteFile(sp, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
}

func (x startupFixture) dial(t *testing.T, rejected int, offline map[int]bool) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	addresses := map[string]string{}
	for i := range 3 {
		if offline[i] {
			continue
		}
		member := testMember(x.r.Cluster, i)
		var handler http.Handler
		if i == rejected {
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
		} else {
			private := append(ed25519.PrivateKey(nil), x.f.private[i]...)
			session, err := NewProofSession()
			if err != nil {
				t.Fatal(err)
			}
			handler, err = NewIdentityHandler(x.r.Cluster, member, session, private, func(ctx context.Context, p ProofPurpose) (LocalObservation, error) {
				s, err := ReadLocalVolume(ctx, x.dirs[member.Ordinal], x.r.Cluster, member, "sandbox")
				return LocalObservation{Volume: s.Volume, Snapshot: s.Snapshot, ConfigDigest: s.ConfigDigest}, err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		addresses[member.DNS+":18080"] = strings.TrimPrefix(server.URL, "http://")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		actual, ok := addresses[address]
		if !ok {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("fixture completed unreachable")}
		}
		return (&net.Dialer{}).DialContext(ctx, network, actual)
	}
}

func TestMemberStartupIndependentSentinelAndReplica(t *testing.T) {
	for _, sentinel := range []bool{true, false} {
		t.Run(strconv.FormatBool(sentinel), func(t *testing.T) {
			x := newStartupFixture(t, 1)
			x.rewrite(t, 1, 2, 5, Replica)
			// Recreate a valid retained replica target that disagrees with Sentinel.
			p := filepath.Join(x.dirs[1], "redis.conf")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b = []byte(strings.Replace(string(b), "replicaof "+x.r.Cluster.Members[2], "replicaof "+x.r.Cluster.Members[0], 1))
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			want := "redis.conf"
			if sentinel {
				want = "sentinel.conf"
			}
			path, err := PrepareMemberLaunch(context.Background(), x.options, sentinel)
			if err != nil || path != filepath.Join(x.dirs[1], want) {
				t.Fatal("independent retained member failed to launch", err)
			}
		})
	}
}

func TestMemberStartupNonzeroColdRestore(t *testing.T) {
	x := newStartupFixture(t, 2)
	for i := range 3 {
		role := Replica
		if i == 2 {
			role = Primary
		}
		x.rewrite(t, i, 2, 9, role)
	}
	path, err := prepareMemberLaunch(context.Background(), x.options, false, memberStartupDependencies{dial: x.dial(t, -1, map[int]bool{0: true})})
	if err != nil || path != filepath.Join(x.dirs[2], "redis.conf") {
		t.Fatal("prior nonzero primary failed safe cold restore", err)
	}
}

func TestMemberStartupFormerPrimaryWaitsForOwnSentinelBeforeDowngrade(t *testing.T) {
	x := newStartupFixture(t, 0)
	for i := 1; i < 3; i++ {
		role := Replica
		if i == 2 {
			role = Primary
		}
		x.rewrite(t, i, 2, 9, role)
	}
	sp := filepath.Join(x.dirs[0], "sentinel.conf")
	rp := filepath.Join(x.dirs[0], "redis.conf")
	originalRedis, err := os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	dial := x.dial(t, -1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := prepareMemberLaunch(ctx, x.options, false, memberStartupDependencies{dial: dial})
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatal("former primary launched before own durable monitor caught up", err)
	case <-time.After(350 * time.Millisecond):
	}
	current, err := os.ReadFile(rp)
	if err != nil || string(current) != string(originalRedis) {
		t.Fatal("Redis config changed before own Sentinel agreement", err)
	}
	x.rewrite(t, 0, 2, 9, Primary)
	sentinelBefore, err := os.ReadFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal("downgrade did not launch", err)
	}
	current, err = os.ReadFile(rp)
	if err != nil {
		t.Fatal(err)
	}
	want := string(originalRedis)
	if !strings.HasSuffix(want, "\n") {
		want += "\n"
	}
	want += "replicaof " + x.r.Cluster.Members[2] + " 6379\n"
	if string(current) != want {
		t.Fatal("downgrade did not retain every other Redis config byte")
	}
	sentinelAfter, err := os.ReadFile(sp)
	if err != nil || string(sentinelAfter) != string(sentinelBefore) {
		t.Fatal("startup wrote running Sentinel config", err)
	}
}

func TestMemberStartupPrimaryRefusesInsufficientAndRejectedEvidence(t *testing.T) {
	for _, name := range []string{"rejected", "two lost", "two lost volumes", "higher own minority", "same epoch foreign", "wrong peer id", "initial replica missing", "incomplete dial"} {
		t.Run(name, func(t *testing.T) {
			x := newStartupFixture(t, 0)
			rejected := -1
			offline := map[int]bool{}
			switch name {
			case "rejected":
				rejected = 1
			case "two lost":
				offline[1] = true
				offline[2] = true
			case "two lost volumes":
				x.dirs[1] = t.TempDir()
				x.dirs[2] = t.TempDir()
			case "higher own minority":
				x.rewrite(t, 0, 0, 10, Primary)
			case "same epoch foreign":
				x.rewrite(t, 1, 2, 0, Replica)
				x.rewrite(t, 2, 2, 0, Primary)
			case "wrong peer id":
				p := filepath.Join(x.dirs[1], "sentinel.conf")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				b = []byte(strings.Replace(string(b), fixedSentinelID(x.f.keys[1]), strings.Repeat("a", 40), 1))
				if err := os.WriteFile(p, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "initial replica missing":
				offline[1] = true
			}
			dial := x.dial(t, rejected, offline)
			if name == "incomplete dial" {
				dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			path, err := prepareMemberLaunch(context.Background(), x.options, false, memberStartupDependencies{dial: dial})
			if err == nil || path != "" {
				t.Fatal("prior primary launched with refused evidence")
			}
			b, readErr := os.ReadFile(filepath.Join(x.dirs[0], "redis.conf"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(b), "replicaof ") {
				t.Fatal("denied primary mutated config")
			}
		})
	}
}

func TestMemberStartupInitialPrimaryUsesConfiguredReplicaProofsWithoutRedisProcess(t *testing.T) {
	x := newStartupFixture(t, 0)
	path, err := prepareMemberLaunch(context.Background(), x.options, false, memberStartupDependencies{dial: x.dial(t, -1, nil)})
	if err != nil || path != filepath.Join(x.dirs[0], "redis.conf") {
		t.Fatal("initial primary waited on Redis processes instead of configured replica inventory", err)
	}
}

func TestMemberStartupRegisteredEmptyReplacementAndCancellation(t *testing.T) {
	for _, phase := range []Phase{Pending, Initialized} {
		t.Run(string(phase), func(t *testing.T) {
			x := newStartupFixture(t, 1)
			x.r.Cluster.Phase = phase
			x.writeControls(t)
			x.options.Directory = t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if path, err := PrepareMemberLaunch(ctx, x.options, false); !errors.Is(err, context.Canceled) || path != "" {
				t.Fatal("cancellation not honored", err)
			}
			x.options.Timeout = 30 * time.Millisecond
			if path, err := PrepareMemberLaunch(context.Background(), x.options, false); err == nil || path != "" {
				t.Fatal("registered empty replacement launched")
			}
			entries, err := os.ReadDir(x.options.Directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("replacement acquired new marker or files", err)
			}
		})
	}
}

func TestMemberStartupConcurrentGenuineEmptyReservationThenRegistration(t *testing.T) {
	x := newStartupFixture(t, 1)
	x.options.Directory = t.TempDir()
	x.options.Timeout = 2 * time.Second
	if err := os.Remove(x.options.Files.Registration); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 2)
	for _, sentinel := range []bool{false, true} {
		go func() { _, err := PrepareMemberLaunch(ctx, x.options, sentinel); results <- err }()
	}
	var local LocalVolumeSnapshot
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var err error
		local, err = ReadLocalVolume(context.Background(), x.options.Directory, x.r.Cluster, testMember(x.r.Cluster, 1), "sandbox")
		if err == nil && local.Volume.Identity != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if local.Volume.Identity == nil || local.Volume.Identity.InitialConfig != Reserved {
		t.Fatal("concurrent wrappers did not reserve genuine empty PVC")
	}
	x.f.proofs[1] = x.f.sign(t, 1, x.f.challenges[1], LocalObservation{Volume: local.Volume})
	r, err := RegisterFreshVolumes(x.r.Cluster, x.f.keys, x.f.challenges, x.f.proofs)
	if err != nil {
		t.Fatal(err)
	}
	x.r = r
	x.writeControls(t)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("registered same-marker wrappers did not complete", err)
		}
	}
	parts, err := readRetainedParts(context.Background(), x.options.Directory, x.r, x.f.keys, testMember(x.r.Cluster, 1), "sandbox")
	if err != nil || parts.identity.MarkerID != local.Volume.Identity.MarkerID || parts.identity.InitialConfig != Configured {
		t.Fatal("wrapper transaction replaced reservation or failed confirmation", err)
	}
}

func TestMemberStartupFinalConfirmationRejectsChangedInodeAndCancellation(t *testing.T) {
	for _, sentinel := range []bool{false, true} {
		for _, target := range []string{"identity.json", "redis.conf", "sentinel.conf"} {
			t.Run(strconv.FormatBool(sentinel)+"/"+target, func(t *testing.T) {
				x := newStartupFixture(t, 1)
				m := testMember(x.r.Cluster, 1)
				parts, err := readRetainedParts(context.Background(), x.dirs[1], x.r, x.f.keys, m, "sandbox")
				if err != nil {
					t.Fatal(err)
				}
				p := filepath.Join(x.dirs[1], target)
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(x.dirs[1], "replacement")
				if err := os.WriteFile(replacement, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, p); err != nil {
					t.Fatal(err)
				}
				if path, err := confirmMemberLaunch(context.Background(), x.options, x.r, x.f.keys, m, parts, sentinel, nil); err == nil || path != "" {
					t.Fatal("same-byte replacement inode authorized launch")
				}
			})
		}
	}
}

func TestMemberStartupPublicRejectsInvalidConfiguredOptions(t *testing.T) {
	for _, name := range []string{"data password", "sentinel password", "equal passwords", "down timeout", "failover timeout", "parallel syncs"} {
		t.Run(name, func(t *testing.T) {
			x := newStartupFixture(t, 1)
			switch name {
			case "data password":
				x.options.Initial.DataPassword = ""
			case "sentinel password":
				x.options.Initial.SentinelPassword = ""
			case "equal passwords":
				x.options.Initial.SentinelPassword = x.options.Initial.DataPassword
			case "down timeout":
				x.options.Initial.DownAfterMilliseconds = 1
			case "failover timeout":
				x.options.Initial.FailoverTimeoutMilliseconds = 1
			case "parallel syncs":
				x.options.Initial.ParallelSyncs = 3
			}
			if path, err := PrepareMemberLaunch(context.Background(), x.options, true); err == nil || path != "" {
				t.Fatal("invalid configured public API options authorized launch")
			}
		})
	}
}

func TestMemberStartupFinalCheckpointRevalidatesSentinelAndReplica(t *testing.T) {
	for _, sentinel := range []bool{true, false} {
		t.Run(strconv.FormatBool(sentinel), func(t *testing.T) {
			x := newStartupFixture(t, 1)
			member := testMember(x.r.Cluster, 1)
			parts, err := readRetainedParts(context.Background(), x.dirs[1], x.r, x.f.keys, member, "sandbox")
			if err != nil {
				t.Fatal(err)
			}
			hook := func(point string) error {
				if point == "member-before-return" {
					target := filepath.Join(x.dirs[1], "redis.conf")
					b, err := os.ReadFile(target)
					if err != nil {
						return err
					}
					temp := filepath.Join(x.dirs[1], "replacement")
					if err := os.WriteFile(temp, b, 0600); err != nil {
						return err
					}
					return os.Rename(temp, target)
				}
				return nil
			}
			if path, err := confirmMemberLaunchWithHook(context.Background(), x.options, x.r, x.f.keys, member, parts, sentinel, nil, hook); err == nil || path != "" {
				t.Fatal("final checkpoint adopted unsynced replacement inode")
			}
		})
	}
}

func TestMemberStartupFollowTransactionFailuresPreserveOwnedAndUnknownFiles(t *testing.T) {
	for _, point := range []string{"follow-temp-synced", "follow-before-rename", "follow-renamed"} {
		for _, change := range []string{"interrupt", "identity replacement", "temp replacement", "Redis replacement"} {
			t.Run(point+"/"+change, func(t *testing.T) {
				x := newStartupFixture(t, 0)
				x.rewrite(t, 0, 2, 9, Primary)
				m := testMember(x.r.Cluster, 0)
				parts, err := readRetainedParts(context.Background(), x.dirs[0], x.r, x.f.keys, m, "sandbox")
				if err != nil {
					t.Fatal(err)
				}
				originalRedis := string(parts.files[1].data)
				unknownPath := ""
				changed := false
				hook := func(at string) error {
					if at != point {
						return nil
					}
					changed = true
					if change == "interrupt" {
						return errors.New("injected failure")
					}
					target := "identity.json"
					if change == "Redis replacement" {
						target = "redis.conf"
					}
					if change == "temp replacement" {
						entries, err := os.ReadDir(x.dirs[0])
						if err != nil {
							return err
						}
						target = ""
						for _, e := range entries {
							if strings.HasPrefix(e.Name(), ".bootstrap-follow-") {
								target = e.Name()
							}
						}
						if target == "" {
							return errors.New("no temp after rename")
						}
					}
					path := filepath.Join(x.dirs[0], target)
					b, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					replacement := filepath.Join(x.dirs[0], "replacement")
					if err := os.WriteFile(replacement, b, 0600); err != nil {
						return err
					}
					if err := os.Rename(replacement, path); err != nil {
						return err
					}
					unknownPath = path
					return nil
				}
				selected := SelectedEvidence{PrimaryDNS: x.r.Cluster.Members[2], Epoch: 9}
				path, err := confirmMemberLaunchWithHook(context.Background(), x.options, x.r, x.f.keys, m, parts, false, &selected, hook)
				if !changed || err == nil || path != "" {
					t.Fatal("uncertain follow transaction authorized launch", err)
				}
				if unknownPath != "" {
					if _, err := os.Lstat(unknownPath); err != nil {
						t.Fatal("cleanup unlinked unknown replacement", err)
					}
				}
				if point != "follow-renamed" {
					b, err := os.ReadFile(filepath.Join(x.dirs[0], "redis.conf"))
					if err != nil || string(b) != originalRedis {
						t.Fatal("pre-rename failure changed Redis config", err)
					}
				}
				entries, err := os.ReadDir(x.dirs[0])
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".bootstrap-follow-") && filepath.Join(x.dirs[0], e.Name()) != unknownPath {
						t.Fatal("failure leaked known owned follow temp")
					}
				}
			})
		}
	}
}

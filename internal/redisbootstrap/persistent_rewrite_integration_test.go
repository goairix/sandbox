package redisbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	redislib "github.com/redis/go-redis/v9"
)

// This test is run only in its cache-only network-none Docker fixture. It tests
// actual config rewrite/cold authentication, not replication or HA acceptance.
func TestPersistentRealRewriteIntegration(t *testing.T) {
	owner := os.Getenv("TEST_REDIS_BOOTSTRAP_REWRITE_OWNER")
	if owner == "" {
		t.Skip("isolated real Redis rewrite fixture not enabled")
	}
	hostname, err := os.Hostname()
	if err != nil || hostname != owner || !strings.HasPrefix(owner, "sandbox-sentinel-rewrite-") {
		t.Fatal("invalid isolated rewrite fixture owner")
	}
	if _, err := os.Stat("/usr/local/bin/redis-server"); err != nil {
		t.Fatal("isolated Redis executable unavailable")
	}
	r, _, options := initialConfigFixture(t)
	secret, err := GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	r.KeyDigest = digest
	// The fixture supplies exactly these local-only /etc/hosts entries. No
	// external names, production credentials or arbitrary endpoints are used.
	r.Cluster.Members = [3]string{"redis-0", "redis-1", "redis-2"}
	identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[0], Member: Member{DNS: r.Cluster.Members[0], Ordinal: 0}, InitialConfig: Reserved}
	redisConfig, sentinelConfig, err := RenderInitialMemberConfigs(r, keys, identity, options)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	redisPath, sentinelPath := filepath.Join(directory, "redis.conf"), filepath.Join(directory, "sentinel.conf")
	for _, file := range []struct {
		path string
		data []byte
	}{{redisPath, redisConfig}, {sentinelPath, sentinelConfig}} {
		if err := os.WriteFile(file.path, file.data, 0600); err != nil {
			t.Fatal("cannot write private isolated config")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	startPair := func() (func(), string) {
		stopData := startRewriteProcess(t, ctx, redisPath, false)
		stopSentinel := startRewriteProcess(t, ctx, sentinelPath, true)
		stop := func() { stopSentinel(); stopData() }
		t.Cleanup(stop)
		data := rewriteClient("127.0.0.1:6379", options.DataPassword)
		sentinel := rewriteClient("127.0.0.1:26379", options.SentinelPassword)
		t.Cleanup(func() {
			if err := data.Close(); err != nil {
				t.Error("data client close failed")
			}
			if err := sentinel.Close(); err != nil {
				t.Error("Sentinel client close failed")
			}
		})
		for _, client := range []*redislib.Client{data, sentinel} {
			deadline := time.Now().Add(10 * time.Second)
			for client.Ping(ctx).Err() != nil {
				if ctx.Err() != nil || time.Now().After(deadline) {
					t.Fatal("isolated Redis/Sentinel did not start")
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		for _, target := range []struct{ addr, wrong string }{{"127.0.0.1:6379", options.SentinelPassword}, {"127.0.0.1:26379", options.DataPassword}} {
			for _, password := range []string{"", target.wrong} {
				client := rewriteClient(target.addr, password)
				err := client.Ping(ctx).Err()
				closeErr := client.Close()
				if closeErr != nil || err == nil || (!strings.Contains(err.Error(), "NOAUTH") && !strings.Contains(err.Error(), "WRONGPASS")) {
					t.Fatal("isolated endpoint accepted missing/cross credentials or failed for an unrelated reason")
				}
			}
		}
		info, err := data.Info(ctx, "server").Result()
		if err != nil {
			t.Fatal("authenticated server INFO failed")
		}
		var runID string
		for _, line := range strings.Split(info, "\r\n") {
			if strings.HasPrefix(line, "run_id:") {
				runID = strings.TrimPrefix(line, "run_id:")
			}
		}
		if !lowerHex(runID, 20) {
			t.Fatal("invalid real run_id")
		}
		if data.Do(ctx, "CONFIG", "REWRITE").Err() != nil || sentinel.Do(ctx, "SENTINEL", "FLUSHCONFIG").Err() != nil {
			t.Fatal("real configuration rewrite failed")
		}
		return stop, runID
	}
	stop, firstRunID := startPair()
	stop()
	readSnapshot := func() PersistentConfigSnapshot {
		actualRedis, err := os.ReadFile(redisPath)
		if err != nil {
			t.Fatal("cannot read rewritten Redis config")
		}
		actualSentinel, err := os.ReadFile(sentinelPath)
		if err != nil {
			t.Fatal("cannot read rewritten Sentinel config")
		}
		for _, config := range []struct {
			name string
			data []byte
		}{{"Redis", actualRedis}, {"Sentinel", actualSentinel}} {
			for _, line := range strings.Split(string(config.data), "\n") {
				if !strings.HasPrefix(line, "user ") {
					continue
				}
				var safe []string
				for _, token := range strings.Fields(line) {
					switch token {
					case "user", "default", "on", "off", "nopass", "sanitize-payload", "skip-sanitize-payload", "~*", "&*", "+@all":
						safe = append(safe, token)
					default:
						safe = append(safe, "<redacted>")
					}
				}
				t.Logf("actual %s rewrite ACL shape: %s", config.name, strings.Join(safe, " "))
			}
		}
		snapshot, err := ParsePersistentConfigs(r.Cluster, identity.Member, options.MasterName, actualRedis, actualSentinel)
		if err != nil {
			t.Fatalf("real rewritten config rejected: %v", err)
		}
		return snapshot
	}
	first := readSnapshot()
	configured := identity
	configured.InitialConfig = Configured
	if err := WriteVolumeIdentity(ctx, filepath.Join(directory, "identity.json"), configured, r.Cluster); err != nil {
		t.Fatal("cannot install isolated configured identity")
	}
	provider, err := NewLocalObserver(LocalObserverOptions{Directory: directory, Cluster: r.Cluster, Member: identity.Member, MasterName: options.MasterName, Password: options.DataPassword})
	if err != nil {
		t.Fatal("cannot construct actual local observer")
	}
	inventory, err := provider(ctx, InventoryProof)
	if err != nil || inventory.Volume.Identity == nil || inventory.Volume.Identity.InitialConfig != Configured || inventory.RunID != "" {
		_, readErr := ReadLocalVolume(ctx, directory, r.Cluster, identity.Member, options.MasterName)
		for _, name := range []string{"identity.json", "redis.conf", "sentinel.conf"} {
			info, statErr := os.Stat(filepath.Join(directory, name))
			if statErr == nil {
				t.Logf("isolated %s mode=%s size=%d", name, info.Mode(), info.Size())
			}
		}
		t.Fatalf("cold PVC inventory failed without Redis: %v", readErr)
	}
	controlDir := t.TempDir()
	clusterJSON, err := json.Marshal(r.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	registrationJSON, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"cluster.json", clusterJSON}, {"registration.json", registrationJSON}, {"public-keys.json", secret.Data["public-keys.json"]}, {"seed", secret.Data["redis-sentinel-0"]}} {
		if err := os.WriteFile(filepath.Join(controlDir, file.name), file.data, 0600); err != nil {
			t.Fatal("cannot write isolated sidecar control files")
		}
	}
	identityCommand := exec.CommandContext(ctx, "/fixture/redis-bootstrap", "attestor", "-data-dir", directory, "-cluster-file", filepath.Join(controlDir, "cluster.json"), "-registration-file", filepath.Join(controlDir, "registration.json"), "-public-keys-file", filepath.Join(controlDir, "public-keys.json"), "-private-seed-file", filepath.Join(controlDir, "seed"), "-ordinal", "0", "-master-name", options.MasterName)
	identityCommand.Env = append(os.Environ(), "REDIS_PASSWORD="+options.DataPassword)
	stopIdentity := startRewriteCommand(t, identityCommand)
	t.Cleanup(stopIdentity)
	ch, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := FetchIdentityProof(ctx, r.Cluster, identity.Member, keys[0], ch, &configured, nil)
		if err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			t.Fatal("actual sidecar cold inventory unavailable")
		}
		time.Sleep(20 * time.Millisecond)
		// Do not reuse a nonce after a failed/uncertain request.
		ch, err = NewIdentityChallenge(InventoryProof)
		if err != nil {
			t.Fatal(err)
		}
	}
	stop, secondRunID := startPair()
	live, err := provider(ctx, LiveProof)
	if err != nil || live.RunID != secondRunID || live.Snapshot == nil || *live.Snapshot != first {
		t.Fatalf("actual authenticated live observation failed: %v", err)
	}
	currentID, err := readLocalRedisRunID(ctx, "redis-0:6379", options.DataPassword)
	if err != nil {
		t.Fatal("fresh fixed-DNS authenticated INFO failed")
	}
	current := AuthenticatedEndpoint{Member: identity.Member, RunID: currentID, Authenticated: true}
	ch, err = NewIdentityChallenge(LiveProof)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := FetchIdentityProof(ctx, r.Cluster, identity.Member, keys[0], ch, &configured, &current)
	if err != nil || proof.Observation.RunID != secondRunID {
		t.Fatalf("actual signed sidecar live process proof failed: %v", err)
	}
	if status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", identityCommand.Process.Pid)); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				t.Log("isolated sidecar sample:", line)
			}
		}
	}
	stopIdentity()
	stop()
	second := readSnapshot()
	if firstRunID == secondRunID || first != second {
		t.Fatal("cold restart reused run_id or changed retained identity/topology")
	}
	t.Log("production initial configs: real Redis/Sentinel rewrite, authenticated cold restart and retained snapshot passed; not HA acceptance")
}

func rewriteClient(addr, password string) *redislib.Client {
	return redislib.NewClient(&redislib.Options{Addr: addr, Password: password, Protocol: 2, DisableIdentity: true, MaxRetries: -1, ContextTimeoutEnabled: true, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
}

func startRewriteProcess(t *testing.T, ctx context.Context, path string, sentinel bool) func() {
	t.Helper()
	args := []string{path}
	if sentinel {
		args = append(args, "--sentinel")
	}
	// Redis rewrite does not preserve the original mode: it uses 0644 & ~umask.
	// The production final-exec wrapper must likewise set a private 0077 umask.
	// This fixed fixture script only passes the trusted test path as argv, never
	// interpolates credentials or tenant input, and exec leaves Redis as the PID.
	cmd := exec.CommandContext(ctx, "/bin/sh", append([]string{"-ec", "umask 077; exec /usr/local/bin/redis-server \"$@\"", "rewrite-fixture"}, args...)...)
	return startRewriteCommand(t, cmd)
}

func startRewriteCommand(t *testing.T, cmd *exec.Cmd) func() {
	t.Helper()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal("cannot start isolated Redis process")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error("isolated process termination signal failed")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error("isolated Redis process did not terminate cleanly")
			}
		case <-time.After(3 * time.Second):
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("isolated process kill failed")
			}
			<-done
			t.Error("isolated Redis termination timed out")
		}
	}
}

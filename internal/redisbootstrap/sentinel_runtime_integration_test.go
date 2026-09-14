package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	redislib "github.com/redis/go-redis/v9"
	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const fixtureDataPassword = "fixture_data_012345678901234567890123456789"
const fixtureSentinelPassword = "fixture_sentinel_012345678901234567890123456789"

// The script gives each node its own container/network namespace/PVC. This
// control listener exists ONLY in the explicitly enabled isolated test binary;
// it is not a production port, API, proxy, election or startup escape hatch.
func TestSentinelRuntimeNode(t *testing.T) {
	if os.Getenv("TEST_SENTINEL_ROLE") != "node" {
		t.Skip("isolated three-container node fixture not enabled")
	}
	checkSentinelFixtureOwner(t)
	ordinal, err := strconv.Atoi(os.Getenv("TEST_SENTINEL_ORDINAL"))
	if err != nil || ordinal < 0 || ordinal > 2 {
		t.Fatal("invalid fixture ordinal")
	}
	var mu sync.Mutex
	processes := map[string]*exec.Cmd{}
	var savedVolume []string
	var savedEpoch []byte
	stop := func(name string) {
		cmd := processes[name]
		if cmd == nil {
			return
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		delete(processes, name)
	}
	start := func(name string) error {
		if processes[name] != nil {
			return fmt.Errorf("fixture process already exists")
		}
		cmd := exec.Command("/app/redis-bootstrap", name, "-ordinal", strconv.Itoa(ordinal), "-master-name", "sandbox")
		if name != "attestor" {
			// Exercise production defaults; a 1s detector can spuriously elect
			// during the intentionally interrupted, sequential fixture startup.
			cmd.Args = append(cmd.Args, "-down-after-milliseconds", "10000", "-failover-timeout-milliseconds", "60000", "-timeout", "70s")
		}
		cmd.Env = append(os.Environ(), "REDIS_PASSWORD="+fixtureDataPassword, "REDIS_SENTINEL_PASSWORD="+fixtureSentinelPassword)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 999, Gid: 999}}
		// Discard raw Redis diagnostics: rewritten config/errors may contain auth.
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("fixture process start failed")
		}
		processes[name] = cmd
		return nil
	}
	prepare := func() error {
		for _, dir := range []string{"/identity-source", "/app"} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
		}
		for _, file := range []struct {
			source, target string
			mode           os.FileMode
		}{{"/fixture-own/seed", "/identity-source/seed", 0400}, {"/fixture/redis-bootstrap", "/app/redis-bootstrap", 0555}} {
			data, err := os.ReadFile(file.source)
			if err != nil || os.WriteFile(file.target, data, file.mode) != nil || os.Chmod(file.target, file.mode) != nil {
				return fmt.Errorf("fixture source preparation failed")
			}
		}
		cmd := exec.Command("/app/redis-bootstrap", "prepare-pod", "-ordinal", strconv.Itoa(ordinal))
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		return cmd.Run()
	}
	emptyVolume := func() error {
		if len(processes) != 0 || len(savedVolume) != 0 {
			return fmt.Errorf("fixture volume processes are not stopped")
		}
		entries, err := os.ReadDir("/data")
		if err != nil || len(entries) == 0 || len(entries) > 16 {
			return fmt.Errorf("fixture retained volume is unconfirmed")
		}
		allowed := map[string]bool{"identity.json": true, "redis.conf": true, "sentinel.conf": true, "initial-config.json": true, ".bootstrap-lock": true, "appendonlydir": true, "dump.rdb": true}
		for _, entry := range entries {
			if !allowed[entry.Name()] {
				return fmt.Errorf("unknown fixture volume entry preserved")
			}
		}
		if err := os.Mkdir("/fixture-retained-backup", 0700); err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if err := copySentinelFixtureEntry(filepath.Join("/data", name), filepath.Join("/fixture-retained-backup", name), 0); err != nil {
				return err
			}
			savedVolume = append(savedVolume, name)
		}
		for _, name := range savedVolume {
			if err := os.RemoveAll(filepath.Join("/data", name)); err != nil {
				return err
			}
		}
		return nil
	}
	restoreVolume := func() error {
		if len(processes) != 0 || len(savedVolume) == 0 {
			return fmt.Errorf("fixture restore is unconfirmed")
		}
		entries, err := os.ReadDir("/data")
		if err != nil || len(entries) != 0 {
			return fmt.Errorf("replacement volume was modified")
		}
		for _, name := range savedVolume {
			if err := copySentinelFixtureEntry(filepath.Join("/fixture-retained-backup", name), filepath.Join("/data", name), 0); err != nil {
				return err
			}
		}
		for _, name := range savedVolume {
			if err := os.RemoveAll(filepath.Join("/fixture-retained-backup", name)); err != nil {
				return err
			}
		}
		savedVolume = nil
		return os.Remove("/fixture-retained-backup")
	}
	server := &http.Server{Addr: ":18181", ReadHeaderTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: 30 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var err error
		switch r.URL.Path {
		case "/prepare":
			err = prepare()
		case "/start":
			for _, name := range []string{"attestor", "redis", "sentinel"} {
				if err = start(name); err != nil {
					break
				}
			}
		case "/stop":
			for _, name := range []string{"redis", "sentinel", "attestor"} {
				stop(name)
			}
		case "/stop-redis":
			stop("redis")
		case "/start-redis":
			err = start("redis")
		case "/stop-sentinel":
			stop("sentinel")
		case "/start-sentinel":
			err = start("sentinel")
		case "/empty-volume":
			err = emptyVolume()
		case "/restore-volume":
			err = restoreVolume()
		case "/fault-high-epoch":
			stop("redis")
			stop("sentinel")
			savedEpoch, err = os.ReadFile("/data/sentinel.conf")
			if err == nil {
				lines := strings.Split(string(savedEpoch), "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, "sentinel config-epoch ") {
						lines[i] = "sentinel config-epoch sandbox 900"
					}
					if strings.HasPrefix(line, "sentinel current-epoch ") {
						lines[i] = "sentinel current-epoch 900"
					}
				}
				err = os.WriteFile("/data/sentinel.conf", []byte(strings.Join(lines, "\n")), 0600)
			}
		case "/restore-epoch":
			if len(savedEpoch) == 0 {
				err = fmt.Errorf("fixture epoch backup missing")
			} else {
				err = os.WriteFile("/data/sentinel.conf", savedEpoch, 0600)
				savedEpoch = nil
			}
		case "/status":
			for _, name := range []string{"attestor", "redis", "sentinel"} {
				cmd := processes[name]
				if cmd == nil || cmd.Process.Signal(syscall.Signal(0)) != nil {
					err = fmt.Errorf("fixture process absent")
				}
			}
		case "/done":
			for _, name := range []string{"redis", "sentinel", "attestor"} {
				stop(name)
			}
			go func() { time.Sleep(50 * time.Millisecond); _ = server.Close() }()
		default:
			err = fmt.Errorf("unknown fixture operation")
		}
		if err != nil {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "fixture operation failed\n")
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal("fixture control listener failed")
	}
}

// Real Redis, Sentinel, wrapper exec and signed identity HTTP are exercised.
// The namespace ConfigMap client here emulates Kubernetes RV CAS and projects
// accepted bytes into a fixture directory; it does NOT claim real API/Helm/CSI
// or Kubernetes-container private-mount enforcement coverage.
func TestSentinelRuntimeThreeMembers(t *testing.T) {
	if os.Getenv("TEST_SENTINEL_ROLE") != "controller" {
		t.Skip("isolated three-member fixture not enabled")
	}
	checkSentinelFixtureOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	cluster := ClusterState{ClusterID: "isolated-real-three-members", Members: [3]string{"redis-0", "redis-1", "redis-2"}, Phase: Pending}
	clusterJSON, err := json.Marshal(cluster)
	if err != nil {
		t.Fatal(err)
	}
	cm := &api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "isolated", Name: "state", UID: "isolated-fixture-uid", ResourceVersion: "1"}, Data: map[string]string{"cluster.json": string(clusterJSON)}}
	client := fake.NewSimpleClientset(cm)
	client.PrependReactor("create", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		accepted := action.(ktesting.CreateAction).GetObject().(*api.Secret).DeepCopy()
		accepted.UID = "isolated-identity-uid"
		accepted.ResourceVersion = "1"
		if err := client.Tracker().Create(api.SchemeGroupVersion.WithResource("secrets"), accepted, "isolated"); err != nil {
			return true, nil, err
		}
		return true, accepted, nil
	})
	identityOptions := IdentitySecretOptions{Namespace: "isolated", StatefulSetName: "redis", SecretName: "redis-identity", StateConfigMap: "state", Members: cluster.Members, FreshClusterID: cluster.ClusterID}
	if err := EnsureIdentitySecret(ctx, client.CoreV1(), identityOptions); err != nil {
		t.Fatal("automatic fixture identity creation failed", err)
	}
	secret, err := client.CoreV1().Secrets("isolated").Get(ctx, "redis-identity", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	identityOptions.FreshClusterID = ""
	if err := EnsureIdentitySecret(ctx, client.CoreV1(), identityOptions); err != nil {
		t.Fatal("automatic identity reuse failed", err)
	}
	creates := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "secrets" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatal("fixture identity was regenerated")
	}
	keys, err := ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal("cannot project isolated fixture control bytes")
		}
	}
	write("/fixture-state/public/public-keys.json", secret.Data["public-keys.json"])
	for i := range 3 {
		write(fmt.Sprintf("/fixture-state/node-%d/seed", i), secret.Data[fmt.Sprintf("redis-%d", i)])
	}
	write("/fixture-state/control/cluster.json", clusterJSON)
	var cmMu sync.Mutex
	grantPublished := make(chan struct{})
	var grantOnce sync.Once
	client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cmMu.Lock()
		defer cmMu.Unlock()
		update := action.(ktesting.UpdateAction).GetObject().(*api.ConfigMap)
		obj, err := client.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), "isolated", "state")
		if err != nil {
			return true, nil, err
		}
		old := obj.(*api.ConfigMap)
		if old.UID != update.UID || old.ResourceVersion != update.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "state", fmt.Errorf("isolated RV conflict"))
		}
		accepted := update.DeepCopy()
		version, err := strconv.Atoi(old.ResourceVersion)
		if err != nil {
			return true, nil, err
		}
		accepted.ResourceVersion = strconv.Itoa(version + 1)
		if err := client.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), accepted, "isolated"); err != nil {
			return true, nil, err
		}
		// registration first while Pending, cluster first on phase transition;
		// coherent readers never accept a torn pair and retry after both land.
		write("/fixture-state/control/registration.json", []byte(accepted.Data["registration.json"]))
		write("/fixture-state/control/cluster.json", []byte(accepted.Data["cluster.json"]))
		grantOnce.Do(func() { close(grantPublished) })
		return true, accepted, nil
	})
	operation := func(i int, path string) {
		t.Helper()
		operationCtx := ctx
		if path == "done" {
			operationCtx = context.Background()
		}
		request, err := http.NewRequestWithContext(operationCtx, http.MethodPost, fmt.Sprintf("http://redis-%d:18181/%s", i, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		h := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := h.Do(request)
		if err != nil {
			t.Fatal("isolated node operation failed:", path)
		}
		_, copyErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		h.CloseIdleConnections()
		if response.StatusCode != 200 || copyErr != nil || closeErr != nil {
			t.Fatal("isolated node operation rejected:", path)
		}
	}
	for i := range 3 {
		operation(i, "prepare")
		operation(i, "start")
	}
	t.Cleanup(func() {
		for i := range 3 {
			operation(i, "done")
		}
	})
	options := InitializeOptions{Namespace: "isolated", Name: "state", PublicKeys: keys, MasterName: "sandbox", DataPassword: fixtureDataPassword, SentinelPassword: fixtureSentinelPassword, AckTimeout: time.Second, Timeout: 50 * time.Second}
	interruptedCtx, interruptedCancel := context.WithCancel(ctx)
	interruptedDone := make(chan error, 1)
	go func() {
		interruptedDone <- InitializeNamespaceBootstrap(interruptedCtx, client.CoreV1().ConfigMaps("isolated"), options)
	}()
	select {
	case <-grantPublished:
		interruptedCancel()
	case <-ctx.Done():
		interruptedCancel()
		t.Fatal("actual fresh registration was not published")
	}
	if err := <-interruptedDone; !errors.Is(err, context.Canceled) {
		t.Fatal("actual interrupted initializer did not preserve cancellation")
	}
	interrupted, err := LoadNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), "isolated", "state")
	if err != nil || interrupted.Cluster.Phase != Pending || interrupted.Registration == nil {
		t.Fatal("actual initial registration was reset or phase opened before verification")
	}
	if err := InitializeNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), options); err != nil {
		describeSentinelRuntime(t, ctx, cluster, keys)
		t.Fatal("actual fresh initialization/replication/ACK failed:", err)
	}
	state, err := LoadNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), "isolated", "state")
	if err != nil || state.Cluster.Phase != Initialized || state.Registration == nil || state.Registration.MarkerIDs != interrupted.Registration.MarkerIDs {
		t.Fatal("actual fresh initialization did not open paired phase gate")
	}
	registration := *state.Registration
	t.Log("PASS: fresh reserve -> three signed registration markers -> durable config -> actual wrapper exec -> replication/ACK -> Initialized CAS")
	t.Log("PASS: initialization cancelled after registration; resumed using same markers without premature phase opening")
	for _, addr := range []string{"redis-0:6379", "redis-0:26379"} {
		correct, wrong := fixtureDataPassword, fixtureSentinelPassword
		if strings.HasSuffix(addr, ":26379") {
			correct, wrong = wrong, correct
		}
		for _, password := range []string{"", wrong, correct} {
			client := sentinelRuntimeClient(addr, password)
			err := client.Ping(ctx).Err()
			closeErr := client.Close()
			if closeErr != nil || (password == correct) != (err == nil) {
				t.Fatal("actual independent endpoint authentication contract failed")
			}
		}
	}
	t.Log("PASS: missing/cross passwords rejected, independent data and Sentinel passwords accepted")
	topology := TopologyOptions{Registration: registration, PublicKeys: keys, MasterName: "sandbox", DataPassword: fixtureDataPassword, SentinelPassword: fixtureSentinelPassword, AckTimeout: time.Second}
	verify := func(all bool) {
		t.Helper()
		deadline := time.Now().Add(65 * time.Second)
		for VerifyTopology(ctx, topology, all) != nil {
			if ctx.Err() != nil || time.Now().After(deadline) {
				describeSentinelRuntime(t, ctx, cluster, keys)
				t.Fatal("actual three-member topology failed to converge")
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	verify(true)
	data := sentinelRuntimeClient("redis-0:6379", fixtureDataPassword)
	conn := data.Conn()
	if err := conn.Do(ctx, "SET", "sandbox:fixture:durable", "present").Err(); err != nil {
		describeSentinelRuntime(t, ctx, cluster, keys)
		t.Fatal("actual business-like fixture SET failed:", err)
	}
	acks, err := conn.Do(ctx, "WAIT", 2, 2000).Int64()
	if err != nil || acks != 2 {
		t.Fatal("actual fixture key not replicated to both replicas")
	}
	if conn.Close() != nil || data.Close() != nil {
		t.Fatal("actual client close failed")
	}
	operation(0, "stop")
	verify(false)
	primary := -1
	for i := 1; i < 3; i++ {
		c := sentinelRuntimeClient(fmt.Sprintf("redis-%d:6379", i), fixtureDataPassword)
		info, err := c.Info(ctx, "replication").Result()
		if c.Close() != nil {
			t.Fatal("actual INFO client close failed")
		}
		if err == nil && strings.Contains(info, "role:master\r\n") {
			primary = i
		}
	}
	if primary < 1 {
		t.Fatal("actual Sentinel did not elect nonzero primary")
	}
	write("/fixture-state/expected-primary", []byte(strconv.Itoa(primary)))
	primaryClient := sentinelRuntimeClient(fmt.Sprintf("redis-%d:6379", primary), fixtureDataPassword)
	value, err := primaryClient.Get(ctx, "sandbox:fixture:durable").Result()
	if primaryClient.Close() != nil || err != nil || value != "present" {
		t.Fatal("actual elected primary lost replicated key")
	}
	t.Log("PASS: actual primary member's Redis/Sentinel/attestor terminated; Sentinel elected nonzero primary with replicated key; initialized verification tolerated one unreachable member")
	operation(0, "start")
	verify(true)
	old := sentinelRuntimeClient("redis-0:6379", fixtureDataPassword)
	info, err := old.Info(ctx, "replication").Result()
	if old.Close() != nil || err != nil || !strings.Contains(info, "role:slave\r\n") {
		t.Fatal("actual old primary did not recover as replica")
	}
	t.Log("PASS: retained old primary wrapper recovered as replica without promotion")
	operation(0, "stop-redis")
	operation(0, "start-redis")
	verify(true)
	t.Log("PASS: actual Redis-only restart retained role and authentication")
	for i := range 3 {
		operation(i, "stop")
	}
	for i := range 3 {
		operation(i, "start")
	}
	verify(true)
	assertSentinelRuntimeRecovery(t, ctx, primary)
	if err := InitializeNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), options); err != nil {
		t.Fatal("actual initialized cold verification failed")
	}
	current, err := LoadNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), "isolated", "state")
	if err != nil || current.ResourceVersion != state.ResourceVersion || current.Registration.MarkerIDs != registration.MarkerIDs {
		t.Fatal("cold verification reset installation state")
	}
	t.Log("PASS: all Redis/Sentinel/attestor processes stopped, cold wrappers restored nonzero topology; original markers and state RV unchanged")
	minority := (primary + 1) % 3
	operation(minority, "fault-high-epoch")
	highChallenge, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	highProof, err := FetchIdentityProof(ctx, cluster, Member{DNS: cluster.Members[minority], Ordinal: minority}, keys[minority], highChallenge, nil, nil)
	if err != nil || highProof.Observation.Snapshot == nil || highProof.Observation.Snapshot.State.SentinelEpoch != 900 {
		t.Fatal("actual higher-minority negative precondition was not signed")
	}
	if VerifyTopology(ctx, topology, false) == nil {
		t.Fatal("actual reachable signed higher minority epoch was ignored")
	}
	operation(minority, "restore-epoch")
	operation(minority, "start-redis")
	operation(minority, "start-sentinel")
	verify(true)
	t.Log("PASS: reachable actual signed higher single-member persisted epoch blocked older majority; restored isolated fixture converged")
	operation(0, "stop")
	operation(0, "empty-volume")
	operation(0, "start")
	ch, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		proof, err := FetchIdentityProof(ctx, cluster, Member{DNS: "redis-0", Ordinal: 0}, keys[0], ch, nil, nil)
		if err == nil && proof.Observation.Volume.Empty {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual same-name empty replacement did not expose empty inventory")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if VerifyTopology(ctx, topology, false) == nil {
		t.Fatal("actual registered empty replacement accepted")
	}
	operation(1, "stop")
	operation(1, "empty-volume")
	operation(1, "start")
	if VerifyTopology(ctx, topology, false) == nil {
		t.Fatal("actual two missing member states accepted")
	}
	for _, i := range []int{0, 1} {
		operation(i, "stop")
		operation(i, "restore-volume")
		operation(i, "start")
	}
	verify(true)
	t.Log("PASS: actual same-DNS emptied retained volume and two missing state sets refused; blocked wrappers did not seed replacement data")
	for i := range 3 {
		if i != primary {
			operation(i, "stop-redis")
		}
	}
	if VerifyTopology(ctx, topology, false) == nil {
		t.Fatal("actual lack of replica ACK was accepted")
	}
	t.Log("PASS: two data nodes unavailable -> replication/ACK verification refused")
}

func sentinelRuntimeClient(addr, password string) *redislib.Client {
	return redislib.NewClient(&redislib.Options{Addr: addr, Password: password, Protocol: 2, MaxRetries: -1, DialerRetries: 1, PoolSize: 1, MinIdleConns: 0, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
}

// Copies only stopped, bounded, whitelisted fixture data between the owned PVC
// and owned container overlay (rename would cross filesystems). No production
// path reaches this helper. Metadata is retained for strict UID999/0600 readers.
func copySentinelFixtureEntry(source, target string, depth int) error {
	if depth > 1 {
		return fmt.Errorf("unexpected fixture data nesting")
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe fixture backup entry")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != 0 && stat.Uid != 999) {
		return fmt.Errorf("unknown fixture backup owner")
	}
	if info.IsDir() {
		entries, err := os.ReadDir(source)
		if err != nil || len(entries) > 32 {
			return fmt.Errorf("fixture backup directory is unbounded")
		}
		if err := os.Mkdir(target, 0700); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copySentinelFixtureEntry(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()), depth+1); err != nil {
				return err
			}
		}
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chown(target, int(stat.Uid), int(stat.Gid))
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<20 {
		return fmt.Errorf("fixture backup file is unconfirmed")
	}
	in, err := os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, 128<<20+1))
	chownErr, chmodErr := out.Chown(int(stat.Uid), int(stat.Gid)), out.Chmod(info.Mode().Perm())
	syncErr, inClose, outClose := out.Sync(), in.Close(), out.Close()
	if copyErr != nil || chownErr != nil || chmodErr != nil || syncErr != nil || inClose != nil || outClose != nil {
		return fmt.Errorf("fixture backup copy unconfirmed")
	}
	return nil
}

func describeSentinelRuntime(t *testing.T, ctx context.Context, cluster ClusterState, keys [3]ed25519.PublicKey) {
	t.Helper()
	for i := range 3 {
		ch, err := NewIdentityChallenge(InventoryProof)
		if err != nil {
			continue
		}
		proof, err := FetchIdentityProof(ctx, cluster, Member{Ordinal: i, DNS: cluster.Members[i]}, keys[i], ch, nil, nil)
		if err == nil {
			t.Logf("fixture member%d inventory snapshot=%+v", i, proof.Observation.Snapshot)
		} else {
			t.Logf("fixture member%d inventory unavailable", i)
		}
		for _, target := range []struct {
			port     int
			password string
		}{{6379, fixtureDataPassword}, {26379, fixtureSentinelPassword}} {
			c := sentinelRuntimeClient(fmt.Sprintf("redis-%d:%d", i, target.port), target.password)
			if target.port == 6379 {
				info, err := c.Info(ctx, "replication").Result()
				if err == nil {
					t.Logf("fixture member%d data INFO replication=%s", i, info)
				} else {
					t.Logf("fixture member%d data unavailable", i)
				}
			} else {
				result, err := c.Do(ctx, "SENTINEL", "MASTER", "sandbox").Result()
				if err == nil {
					t.Logf("fixture member%d monitor=%v", i, result)
				} else {
					t.Logf("fixture member%d Sentinel unavailable", i)
				}
			}
			_ = c.Close()
		}
	}
}

func checkSentinelFixtureOwner(t *testing.T) {
	t.Helper()
	owner := os.Getenv("TEST_SENTINEL_OWNER")
	hostname, err := os.Hostname()
	if !strings.HasPrefix(owner, "sandbox-sentinel-runtime-") || err != nil || hostname != owner {
		t.Fatal("invalid isolated Sentinel fixture owner")
	}
}

func TestSentinelRuntimeNewIP(t *testing.T) {
	if os.Getenv("TEST_SENTINEL_ROLE") != "controller" {
		t.Skip("isolated new-IP fixture not enabled")
	}
	checkSentinelFixtureOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	files, err := ReadBootstrapFiles(ctx, BootstrapFilePaths{Cluster: "/fixture-state/control/cluster.json", Registration: "/fixture-state/control/registration.json", PublicKeys: "/fixture-state/public/public-keys.json"})
	if err != nil || files.Registration == nil || files.Cluster.Phase != Initialized {
		t.Fatal("new-IP fixture did not retain original initialized identity")
	}
	oldIPs := strings.Split(os.Getenv("TEST_SENTINEL_OLD_IPS"), ",")
	if len(oldIPs) != 3 {
		t.Fatal("new-IP fixture lacks prior endpoint inventory")
	}
	operation := func(i int, path string) {
		t.Helper()
		requestCtx := ctx
		if path == "done" {
			requestCtx = context.Background()
		}
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, fmt.Sprintf("http://redis-%d:18181/%s", i, path), nil)
		if err != nil {
			t.Fatal(err)
		}
		h := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
		response, err := h.Do(request)
		if err != nil {
			t.Fatal("new-IP node operation failed:", path)
		}
		_, copyErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		h.CloseIdleConnections()
		if response.StatusCode != 200 || copyErr != nil || closeErr != nil {
			t.Fatal("new-IP node operation rejected:", path)
		}
	}
	for i := range 3 {
		addresses, err := net.DefaultResolver.LookupHost(ctx, files.Cluster.Members[i])
		if err != nil || len(addresses) != 1 || addresses[0] == oldIPs[i] {
			t.Fatal("node DNS did not actually change IP")
		}
		operation(i, "prepare")
		operation(i, "start")
	}
	t.Cleanup(func() {
		for i := range 3 {
			operation(i, "done")
		}
	})
	o := TopologyOptions{Registration: *files.Registration, PublicKeys: files.PublicKeys, MasterName: "sandbox", DataPassword: fixtureDataPassword, SentinelPassword: fixtureSentinelPassword, AckTimeout: time.Second}
	deadline := time.Now().Add(50 * time.Second)
	for VerifyTopology(ctx, o, true) != nil {
		if ctx.Err() != nil || time.Now().After(deadline) {
			describeSentinelRuntime(t, ctx, files.Cluster, files.PublicKeys)
			t.Fatal("actual changed-IP retained wrappers did not recover")
		}
		time.Sleep(250 * time.Millisecond)
	}
	expected, err := os.ReadFile("/fixture-state/expected-primary")
	if err != nil || len(expected) != 1 || (expected[0] != '1' && expected[0] != '2') {
		t.Fatal("new-IP fixture lacks original elected primary")
	}
	assertSentinelRuntimeRecovery(t, ctx, int(expected[0]-'0'))
	current, err := ReadBootstrapFiles(ctx, BootstrapFilePaths{Cluster: "/fixture-state/control/cluster.json", Registration: "/fixture-state/control/registration.json", PublicKeys: "/fixture-state/public/public-keys.json"})
	if err != nil || current.Cluster != files.Cluster || *current.Registration != *files.Registration {
		t.Fatal("changed-IP recovery rewrote namespace identity")
	}
	t.Log("PASS: all three actual DNS endpoint IPs changed; same PVCs/keys restored nonzero roles, authenticated replication and write ACK, without resetting namespace markers")
}

func assertSentinelRuntimeRecovery(t *testing.T, ctx context.Context, expected int) {
	t.Helper()
	if expected < 1 || expected > 2 {
		t.Fatal("recovery test expected nonzero elected primary")
	}
	for i := range 3 {
		c := sentinelRuntimeClient(fmt.Sprintf("redis-%d:6379", i), fixtureDataPassword)
		info, err := c.Info(ctx, "replication").Result()
		if err != nil || strings.Contains(info, "role:master\r\n") != (i == expected) || (i != expected && !strings.Contains(info, "role:slave\r\n")) {
			_ = c.Close()
			t.Fatal("actual recovery changed retained elected role or reset primary to0")
		}
		value, err := c.Get(ctx, "sandbox:fixture:durable").Result()
		if c.Close() != nil || err != nil || value != "present" {
			t.Fatal("actual recovered process lost replicated fixture key")
		}
	}
}

package redisbootstrap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentityServerActualTCPHealthAndProof(t *testing.T) {
	pub, key, cluster, member, challenge, observation := proofFixture(t, InventoryProof)
	session, err := NewProofSession()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler, err := NewIdentityHandler(cluster, member, session, key, func(context.Context, ProofPurpose) (LocalObservation, error) {
		calls.Add(1)
		return observation, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	url, cancel, done := startIdentityServerTest(t, handler)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || calls.Load() != 0 {
		t.Fatal("health must not invoke observer", response.StatusCode, calls.Load())
	}
	proof, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), url+"/v1/identity", cluster, member, pub, challenge, observation.Volume.Identity, nil)
	if err != nil || proof.Session != session || calls.Load() != 1 {
		t.Fatal("actual TCP identity proof failed", err, calls.Load())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("must return context cancellation", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("identity server failed to stop")
	}
}

func TestIdentityServerConnectionAdmissionAndCloseOnce(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newIdentityLimitedListener(raw)
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 34)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	var serverConnections []net.Conn
	t.Cleanup(func() {
		for _, conn := range serverConnections {
			_ = conn.Close()
		}
	})
	connect := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", raw.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	waitAccepted := func() net.Conn {
		t.Helper()
		select {
		case conn := <-accepted:
			serverConnections = append(serverConnections, conn)
			return conn
		case <-time.After(time.Second):
			t.Fatal("accepted connection queued indefinitely")
			return nil
		}
	}
	for i := 0; i < 32; i++ {
		connect()
		waitAccepted()
	}
	assertRejected := func(conn net.Conn) {
		t.Helper()
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, err := conn.Read(make([]byte, 1))
		var timeout net.Error
		if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
			t.Fatal("saturated listener queued instead of closing new connection", err)
		}
		select {
		case conn := <-accepted:
			_ = conn.Close()
			t.Fatal("saturated listener admitted 33rd connection")
		default:
		}
	}
	assertRejected(connect())
	closed := make(chan struct{})
	go func() { _ = serverConnections[0].Close(); _ = serverConnections[0].Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("second Close blocked or returned slot twice")
	}
	connect()
	waitAccepted()
	assertRejected(connect())
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acceptDone:
	case <-time.After(time.Second):
		t.Fatal("listener close did not unblock Accept")
	}
}

func startIdentityServerTest(t *testing.T, handler http.Handler) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = listener.Close() })
	done := make(chan error, 1)
	go func() { done <- runIdentityServer(ctx, listener, handler) }()
	select {
	case err := <-done:
		t.Fatalf("server returned before serving: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	return "http://" + listener.Addr().String(), cancel, done
}

func TestIdentityServerInvalidProductionOptions(t *testing.T) {
	_, key, cluster, member, _, _ := proofFixture(t, InventoryProof)
	options := IdentityServerOptions{Observer: LocalObserverOptions{Directory: "/not-opened-at-start", Cluster: cluster, Member: member, MasterName: "main", Password: strings.Repeat("a", 32)}, PrivateKey: key}
	for _, invalid := range []string{"key", "observer"} {
		t.Run(invalid, func(t *testing.T) {
			bad := options
			if invalid == "key" {
				bad.PrivateKey = nil
			} else {
				bad.Observer.Directory = "relative"
			}
			if err := RunIdentityServer(context.Background(), bad); err == nil || strings.Contains(err.Error(), options.Observer.Password) {
				t.Fatal("invalid production options accepted or leaked", err)
			}
		})
	}
	defer func() {
		if recover() != nil {
			t.Error("nil context panicked instead of failing closed")
		}
	}()
	if err := RunIdentityServer(nil, options); err == nil { //nolint:staticcheck // Deliberately malformed context must fail closed, not panic.
		t.Fatal("nil context must fail closed")
	}
}

func TestIdentityServerProductionObserverReadsActualPVC(t *testing.T) {
	pub, key, _, _, challenge, _ := proofFixture(t, InventoryProof)
	directory, cluster, member := configuredLocalFixture(t)
	before, err := ReadLocalVolume(context.Background(), directory, cluster, member, "main")
	if err != nil {
		t.Fatal(err)
	}
	options := IdentityServerOptions{Observer: LocalObserverOptions{Directory: directory, Cluster: cluster, Member: member, MasterName: "main", Password: strings.Repeat("a", 32)}, PrivateKey: key}
	handler, err := newIdentityServerHandler(options)
	if err != nil {
		t.Fatal("production handler constructor must bind actual local observer", err)
	}
	url, _, _ := startIdentityServerTest(t, handler)
	proof, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), url+"/v1/identity", cluster, member, pub, challenge, before.Volume.Identity, nil)
	if err != nil || proof.Observation.ConfigDigest != before.ConfigDigest || proof.Observation.Snapshot == nil || proof.Observation.Snapshot.State.Role != Primary || proof.Observation.Snapshot.State.PrimaryDNS != member.DNS {
		t.Fatal("production observer did not attest actual nonzero primary PVC", err)
	}
	secondHandler, err := newIdentityServerHandler(options)
	if err != nil {
		t.Fatal(err)
	}
	secondURL, _, _ := startIdentityServerTest(t, secondHandler)
	second, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), secondURL+"/v1/identity", cluster, member, pub, challenge, before.Volume.Identity, nil)
	if err != nil || second.Session == proof.Session {
		t.Fatal("fresh service construction must generate fresh session", err)
	}
	after, err := ReadLocalVolume(context.Background(), directory, cluster, member, "main")
	if err != nil || after.ConfigDigest != before.ConfigDigest || *after.Volume.Identity != *before.Volume.Identity {
		t.Fatal("identity server mutated local retained state", err)
	}
}

func identityServerHandlerFixture(t *testing.T, provider ObservationProvider) http.Handler {
	t.Helper()
	_, key, cluster, member, _, _ := proofFixture(t, InventoryProof)
	session, err := NewProofSession()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewIdentityHandler(cluster, member, session, key, provider)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestIdentityServerHealthStrictAndIndependent(t *testing.T) {
	var calls atomic.Int32
	handler := identityServerHandlerFixture(t, func(context.Context, ProofPurpose) (LocalObservation, error) {
		calls.Add(1)
		return LocalObservation{}, errors.New("uninitialized PVC, offline Redis, unavailable quorum")
	})
	url, _, _ := startIdentityServerTest(t, handler)
	client := &http.Client{Timeout: time.Second}
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/healthz", 200}, {"HEAD", "/healthz", 400}, {"POST", "/healthz", 400},
		{"GET", "/healthz?", 400}, {"GET", "/healthz?secret=redact", 400}, {"GET", "/%68ealthz", 400}, {"GET", "/healthz/", 400},
	} {
		request, err := http.NewRequest(test.method, url+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		if response.StatusCode != test.status || strings.Contains(string(body), "redact") || response.Header.Get("Cache-Control") != "no-store" || !response.Close {
			t.Fatal("health path/method/secret/keepalive bound failed", test, response.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("health path must not inspect PVC, Redis or quorum")
	}
}

func TestIdentityServerSlowHeaderAndBody(t *testing.T) {
	handler := identityServerHandlerFixture(t, func(context.Context, ProofPurpose) (LocalObservation, error) {
		t.Error("partial request reached provider")
		return LocalObservation{}, errIdentityServer
	})
	url, _, _ := startIdentityServerTest(t, handler)
	for _, test := range []struct {
		name, request string
		budget        time.Duration
	}{
		{"header", "GET /healthz HTTP/1.1\r\nHost: localhost\r\nX-Slow:", 2 * time.Second},
		{"body", "POST /v1/identity HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", 3 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(test.budget)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, test.request); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				if response.StatusCode < 400 {
					t.Fatal("partial request accepted", response.StatusCode)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				t.Fatal("server did not bound slow request", err)
			}
		})
	}
}

func TestIdentityServerWriteDeadline(t *testing.T) {
	written := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write(bytes.Repeat([]byte("x"), 16<<20))
		written <- err
	})
	url, _, _ := startIdentityServerTest(t, handler)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET /blocked HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("non-reading client did not reach write deadline")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server write remained blocked")
	}
}

func TestIdentityServerShutdownForcesConnectionClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		<-release // Models an I/O operation that does not honor cancellation.
		close(finished)
	})
	url, cancel, done := startIdentityServerTest(t, handler)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	defer close(release)
	if _, err := io.WriteString(conn, "GET /blocked HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("shutdown did not force HTTP close")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 1))
	var networkError net.Error
	if err == nil || (errors.As(err, &networkError) && networkError.Timeout()) {
		t.Fatal("shutdown kept HTTP connection open")
	}
	select {
	case <-finished:
		t.Fatal("server falsely terminated uncooperative local operation")
	default:
	}
}

func TestIdentityServerCanceledProductionStartDoesNotReadPVC(t *testing.T) {
	_, key, cluster, member, _, _ := proofFixture(t, InventoryProof)
	directory := filepath.Join(t.TempDir(), "absent-pvc")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunIdentityServer(ctx, IdentityServerOptions{Observer: LocalObserverOptions{Directory: directory, Cluster: cluster, Member: member, MasterName: "main", Password: strings.Repeat("a", 32)}, PrivateKey: key})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled valid start must return cancellation without reading PVC", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("service startup mutated PVC", err)
	}
}

func TestIdentityServerUnexpectedServeErrorIsRedacted(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = runIdentityServer(context.Background(), listener, http.NotFoundHandler())
	if err != errIdentityServer || strings.Contains(err.Error(), address) {
		t.Fatal("unexpected serve error leaked endpoint", err)
	}
}

func TestIdentityServerRejectsOversizedHeaders(t *testing.T) {
	handler := identityServerHandlerFixture(t, func(context.Context, ProofPurpose) (LocalObservation, error) {
		t.Error("oversized header reached provider")
		return LocalObservation{}, errIdentityServer
	})
	url, _, _ := startIdentityServerTest(t, handler)
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// net/http adds a bounded parser slop to MaxHeaderBytes. The configured
	// 4096-byte setting is not an exact 4096-byte wire cut-off.
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nX-Large: "+strings.Repeat("x", 16<<10)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatal("oversized header accepted", response.StatusCode)
	}
}

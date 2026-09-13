package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentityHTTPRoundTrip(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	var calls atomic.Int32
	handler, err := NewIdentityHandler(c, m, session, key, func(ctx context.Context, p ProofPurpose) (LocalObservation, error) {
		calls.Add(1)
		if p != LiveProof {
			t.Error("wrong provider purpose")
		}
		return o, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newIdentityHTTPClient()
	p, err := fetchIdentityProof(context.Background(), client, server.URL+"/v1/identity", c, m, pub, ch, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Session != session || calls.Load() != 1 {
		t.Fatal("wrong session or provider calls")
	}
}

func TestIdentityHTTPRejectInvalidRequest(t *testing.T) {
	_, key, c, m, ch, o := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	var calls atomic.Int32
	handler, _ := NewIdentityHandler(c, m, session, key, func(context.Context, ProofPurpose) (LocalObservation, error) { calls.Add(1); return o, nil })
	data, _ := json.Marshal(ch)
	control := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
	control.Header.Set("Content-Type", "application/json")
	controlResponse := httptest.NewRecorder()
	handler.ServeHTTP(controlResponse, control)
	if controlResponse.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatal("valid control did not reach provider", controlResponse.Code, calls.Load())
	}
	tests := []struct{ name, method, path, body, contentType, encoding string }{
		{"method", "GET", "/v1/identity", string(data), "application/json", ""}, {"path", "POST", "/wrong", string(data), "application/json", ""}, {"query", "POST", "/v1/identity?x=y", string(data), "application/json", ""}, {"empty query", "POST", "/v1/identity?", string(data), "application/json", ""},
		{"type", "POST", "/v1/identity", string(data), "text/plain", ""}, {"encoding", "POST", "/v1/identity", string(data), "application/json", "gzip"},
		{"unknown", "POST", "/v1/identity", strings.TrimSuffix(string(data), "}") + `,"secret":"token"}`, "application/json", ""},
		{"duplicate", "POST", "/v1/identity", strings.TrimSuffix(string(data), "}") + `,"version":1}`, "application/json", ""},
		{"case", "POST", "/v1/identity", strings.Replace(string(data), "version", "Version", 1), "application/json", ""},
		{"null", "POST", "/v1/identity", strings.Replace(string(data), "\"version\":1", "\"version\":null", 1), "application/json", ""},
		{"missing", "POST", "/v1/identity", `{"purpose":"inventory","nonce":"` + ch.Nonce + `"}`, "application/json", ""},
		{"trailing", "POST", "/v1/identity", string(data) + `{}`, "application/json", ""}, {"oversize", "POST", "/v1/identity", string(data) + strings.Repeat(" ", 2048), "application/json", ""},
		{"invalid challenge", "POST", "/v1/identity", strings.Replace(string(data), "inventory", "unknown", 1), "application/json", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			if test.encoding != "" {
				req.Header.Set("Content-Encoding", test.encoding)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code < 400 {
				t.Fatal("accepted invalid request")
			}
			if strings.Contains(w.Body.String(), "token") {
				t.Fatal("echoed secret")
			}
			if calls.Load() != 1 {
				t.Fatal("invalid request invoked provider")
			}
		})
	}
	// A present but empty encoding header is independently forbidden; it must
	// not obscure the JSON validation exercised by the matrix above.
	emptyEncoding := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
	emptyEncoding.Header.Set("Content-Type", "application/json")
	emptyEncoding.Header.Set("Content-Encoding", "")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, emptyEncoding)
	if w.Code < 400 || calls.Load() != 1 {
		t.Fatal("empty encoding header accepted or invoked provider")
	}
}

func TestIdentityHTTPRejectMalformedResponse(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	p, err := SignIdentityProof(key, c, m, session, ch, o)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(p)
	responses := map[string]string{"unknown": strings.TrimSuffix(string(data), "}") + `,"x":1}`, "duplicate": strings.TrimSuffix(string(data), "}") + `,"version":1}`, "null": strings.Replace(string(data), `"ordinal":0`, `"ordinal":null`, 1), "nested unknown": strings.Replace(string(data), `"Empty":false`, `"Empty":false,"x":1`, 1), "nested case": strings.Replace(string(data), `"CurrentEpoch"`, `"currentEpoch"`, 1), "nested duplicate": strings.Replace(string(data), `"Empty":false`, `"Empty":false,"Empty":false`, 1), "required pointer null": strings.Replace(string(data), `"volume":`, `"volume":null,"unused":`, 1), "trailing": string(data) + `{}`, "huge": strings.Repeat("x", 16385), "tamper": strings.Replace(string(data), session, strings.Repeat("a", 64), 1)}
	for name, body := range responses {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			if _, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), server.URL, c, m, pub, ch, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}); err == nil {
				t.Fatal("accepted malformed response")
			}
		})
	}
}

func TestIdentityHTTPProviderBounds(t *testing.T) {
	_, key, c, m, ch, _ := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	var active atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	handler, _ := NewIdentityHandler(c, m, session, key, func(ctx context.Context, _ ProofPurpose) (LocalObservation, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-release:
			return LocalObservation{Volume: VolumeState{Empty: true}}, nil
		case <-ctx.Done():
			return LocalObservation{}, ctx.Err()
		}
	})
	data, _ := json.Marshal(ch)
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			r := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
			r.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), r)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("provider not started")
		}
	}
	req := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || active.Load() != 8 {
		t.Error("unbounded provider concurrency")
	}
	close(release)
	for i := 0; i < 8; i++ {
		<-done
	}
	deadlineHandler, _ := NewIdentityHandler(c, m, session, key, func(ctx context.Context, _ ProofPurpose) (LocalObservation, error) {
		<-ctx.Done()
		return LocalObservation{}, fmt.Errorf("private secret token")
	})
	start := time.Now()
	w = httptest.NewRecorder()
	timeoutReq := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
	timeoutReq.Header.Set("Content-Type", "application/json")
	deadlineHandler.ServeHTTP(w, timeoutReq)
	if time.Since(start) > 2500*time.Millisecond || w.Code != http.StatusGatewayTimeout || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("provider timeout not bounded/redacted", w.Code, w.Body.String())
	}
}

func TestIdentityHTTPClientPolicy(t *testing.T) {
	client := newIdentityHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("missing owned transport")
	}
	if tr.Proxy != nil || !tr.DisableCompression || !tr.DisableKeepAlives || client.Timeout > 3*time.Second || client.Timeout <= 0 || tr.ResponseHeaderTimeout <= 0 || tr.MaxResponseHeaderBytes <= 0 {
		t.Fatal("unsafe transport policy")
	}
	pub, _, c, m, ch, o := proofFixture(t, LiveProof)
	current := &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := fetchIdentityProof(context.Background(), client, server.URL, c, m, pub, ch, o.Volume.Identity, current); err == nil || redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchIdentityProof(ctx, client, server.URL, c, m, pub, ch, o.Volume.Identity, current); err == nil {
		t.Fatal("ignored cancellation")
	}
	server.Close()
	if _, err := fetchIdentityProof(context.Background(), client, server.URL, c, m, pub, ch, o.Volume.Identity, current); err == nil {
		t.Fatal("accepted closed server")
	}
}

func TestIdentityHTTPFixedMemberURL(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	handler, _ := NewIdentityHandler(c, m, session, key, func(context.Context, ProofPurpose) (LocalObservation, error) { return o, nil })
	server := httptest.NewServer(handler)
	defer server.Close()
	client := newIdentityHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("missing owned transport")
	}
	dialer := net.Dialer{Timeout: time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != m.DNS+":18080" {
			t.Errorf("wrong destination %s", address)
		}
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	if _, err := fetchIdentityProof(context.Background(), client, "http://"+m.DNS+":18080/v1/identity", c, m, pub, ch, o.Volume.Identity, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityHTTPKeyCopyAndInventoryNulls(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	for _, volume := range []VolumeState{{Empty: true}, {Identity: &VolumeIdentity{ClusterID: c.ClusterID, Member: m, MarkerID: o.Volume.Identity.MarkerID, InitialConfig: Reserved}}} {
		localKey := append([]byte(nil), key...)
		handler, err := NewIdentityHandler(c, m, session, localKey, func(context.Context, ProofPurpose) (LocalObservation, error) {
			return LocalObservation{Volume: volume}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range localKey {
			localKey[i] = 0
		}
		server := httptest.NewServer(handler)
		_, err = fetchIdentityProof(context.Background(), newIdentityHTTPClient(), server.URL+"/v1/identity", c, m, pub, ch, nil, nil)
		server.Close()
		if err != nil {
			t.Fatal("key copy or legitimate optional null rejected", err)
		}
	}
	for name, args := range map[string]struct {
		member   Member
		session  string
		key      []byte
		provider ObservationProvider
	}{"member": {Member{DNS: m.DNS, Ordinal: 1}, session, key, func(context.Context, ProofPurpose) (LocalObservation, error) { return o, nil }}, "session": {m, "bad", key, func(context.Context, ProofPurpose) (LocalObservation, error) { return o, nil }}, "key": {m, session, nil, func(context.Context, ProofPurpose) (LocalObservation, error) { return o, nil }}, "provider": {m, session, key, nil}} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIdentityHandler(c, args.member, args.session, args.key, args.provider); err == nil {
				t.Fatal("accepted invalid handler")
			}
		})
	}
}

func TestIdentityHTTPProviderFailuresAndCancellation(t *testing.T) {
	_, key, c, m, ch, o := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	data, _ := json.Marshal(ch)
	for name, provider := range map[string]ObservationProvider{"error": func(context.Context, ProofPurpose) (LocalObservation, error) {
		return LocalObservation{}, fmt.Errorf("private-key secret configuration")
	}, "invalid observation": func(context.Context, ProofPurpose) (LocalObservation, error) { o.RunID = "invalid"; return o, nil }} {
		t.Run(name, func(t *testing.T) {
			handler, _ := NewIdentityHandler(c, m, session, key, provider)
			request := httptest.NewRequest("POST", "/v1/identity", strings.NewReader(string(data)))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "secret") {
				t.Fatal("provider failure not redacted")
			}
		})
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	handler, _ := NewIdentityHandler(c, m, session, key, func(ctx context.Context, _ ProofPurpose) (LocalObservation, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return LocalObservation{}, ctx.Err()
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fetchIdentityProof(ctx, newIdentityHTTPClient(), server.URL+"/v1/identity", c, m, nil, ch, nil, nil)
		done <- err
	}()
	// An invalid public key must fail before network/provider invocation.
	if err := <-done; err == nil {
		t.Fatal("accepted nil public key")
	}
	pub, _, _, _, _, _ := proofFixture(t, InventoryProof)
	go func() {
		_, err := fetchIdentityProof(ctx, newIdentityHTTPClient(), server.URL+"/v1/identity", c, m, pub, ch, nil, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider not reached")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ignored client cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("client cancellation blocked")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("provider context not canceled")
	}
}

func TestIdentityHTTPResponseHeaderAndBodyBounds(t *testing.T) {
	pub, _, c, m, ch, o := proofFixture(t, LiveProof)
	current := &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}
	for name, serve := range map[string]http.HandlerFunc{
		"wrong type": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "{}")
		},
		"encoding": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, "{}")
		},
		"large headers": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Large", strings.Repeat("a", 32768))
			_, _ = io.WriteString(w, "{}")
		},
		"truncated body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "{}")
		},
		"slow body": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(serve)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			if _, err := fetchIdentityProof(ctx, newIdentityHTTPClient(), server.URL, c, m, pub, ch, o.Volume.Identity, current); err == nil {
				t.Fatal("accepted invalid/unbounded response")
			}
			if time.Since(start) > time.Second {
				t.Fatal("context bound exceeded")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchIdentityProof(ctx, c, m, pub, ch, o.Volume.Identity, current); err == nil {
		t.Fatal("public API ignored canceled context")
	}
	if _, err := FetchIdentityProof(context.Background(), c, Member{DNS: "http://attacker", Ordinal: 0}, pub, ch, o.Volume.Identity, current); err == nil {
		t.Fatal("public API accepted arbitrary destination")
	}
	if _, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), ":invalid", c, m, pub, ch, o.Volume.Identity, current); err == nil {
		t.Fatal("accepted malformed URL")
	}
}

func TestIdentityHTTPStrictJSONSchema(t *testing.T) {
	for _, data := range []string{`null`, `[]`, `{"version":true,"purpose":"inventory","nonce":"x"}`, `{"version":1.0,"purpose":"inventory","nonce":"x"}`, `{"version":1,"purpose":false,"nonce":"x"}`, `{"version":1,"purpose":"inventory","nonce":"x"`, `{"version":1,"purpose":"inventory","nonce":"x",`} {
		var ch IdentityChallenge
		if strictIdentityJSON([]byte(data), &ch) == nil {
			t.Fatal("accepted malformed schema", data)
		}
	}
	if strictIdentityJSON([]byte(`{}`), nil) == nil || strictIdentityJSON([]byte(`{}`), IdentityChallenge{}) == nil {
		t.Fatal("accepted invalid decode target")
	}
	_, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	p, _ := SignIdentityProof(key, c, m, session, ch, o)
	data, _ := json.Marshal(p)
	for _, body := range []string{strings.Replace(string(data), `"Empty":false`, `"Empty":0`, 1), strings.Replace(string(data), `"member":{`, `"member":[] ,"x":{`, 1), strings.Replace(string(data), `"ordinal":0`, `"ordinal":"0"`, 1), strings.Replace(string(data), `"signature":`, `"signature":null,"extra":`, 1)} {
		var decoded IdentityProof
		if strictIdentityJSON([]byte(body), &decoded) == nil {
			t.Fatal("accepted nested invalid schema")
		}
	}
}

func TestIdentityHTTPFreshnessAndExpectedBindings(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	proof, err := SignIdentityProof(key, c, m, session, ch, o)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(proof)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	current := &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}
	staleMarker := *o.Volume.Identity
	staleMarker.MarkerID = strings.Repeat("e", 32)
	next, _ := NewIdentityChallenge(LiveProof)
	for name, args := range map[string]struct {
		challenge IdentityChallenge
		marker    *VolumeIdentity
		endpoint  *AuthenticatedEndpoint
	}{"stale nonce": {next, o.Volume.Identity, current}, "stale marker": {ch, &staleMarker, current}, "stale runid": {ch, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: strings.Repeat("e", 40), Authenticated: true}}, "missing endpoint": {ch, o.Volume.Identity, nil}} {
		t.Run(name, func(t *testing.T) {
			if _, err := fetchIdentityProof(context.Background(), newIdentityHTTPClient(), server.URL, c, m, pub, args.challenge, args.marker, args.endpoint); err == nil {
				t.Fatal("accepted stale/unbound HTTP proof")
			}
		})
	}
}

func TestIdentityHTTPRejectIncoherentPrivateKey(t *testing.T) {
	_, key, c, m, _, o := proofFixture(t, InventoryProof)
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	mixedKey := append(ed25519.PrivateKey(nil), key...)
	copy(mixedKey[ed25519.SeedSize:], otherPublic)
	session, _ := NewProofSession()
	var calls atomic.Int32
	if _, err := NewIdentityHandler(c, m, session, mixedKey, func(context.Context, ProofPurpose) (LocalObservation, error) { calls.Add(1); return o, nil }); err == nil {
		t.Error("constructed signer with incoherent private seed/public halves")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid private key called provider")
	}
}

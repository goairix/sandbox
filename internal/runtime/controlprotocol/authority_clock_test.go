package controlprotocol

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type clockFunc func(context.Context) (ClockObservation, error)

func (f clockFunc) Observe(ctx context.Context) (ClockObservation, error) { return f(ctx) }
func clockFixture(t *testing.T) (TrustBinding, ed25519.PublicKey, ed25519.PrivateKey, time.Time) {
	t.Helper()
	p, k, c, now := managementTestClaims(t)
	return managementTestBinding(c), p, k, now
}
func TestSignedClockService(t *testing.T) {
	binding, pub, key, now := clockFixture(t)
	var calls atomic.Int32
	source := clockFunc(func(context.Context) (ClockObservation, error) {
		calls.Add(1)
		return ClockObservation{UTC: now, Uncertainty: time.Millisecond}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- ServeSignedClock(ctx, listener, SignedClockServerOptions{Binding: binding, Key: key, Source: source})
	}()
	client, err := NewSignedClockClient(SignedClockClientOptions{Binding: binding, Audience: "pid1", PublicKey: pub, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("idle clock called source")
	}
	obs, err := client.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if obs.UTC.Before(now) || obs.UTC.After(now.Add(time.Second)) || obs.Uncertainty < time.Millisecond || obs.Uncertainty > time.Second || calls.Load() != 1 {
		t.Fatalf("bad observation: %+v calls=%d", obs, calls.Load())
	}
	idle, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop/join")
	}
	if _, err := net.DialTimeout("tcp", listener.Addr().String(), 50*time.Millisecond); err == nil {
		t.Fatal("listener still open")
	}
	if calls.Load() != 1 {
		t.Fatal("idle connection called source")
	}
}

// An independent peer signs responses to real framed challenge bytes.
func clockPeer(t *testing.T, mutate func(map[string]any), frame func([]byte) []byte, delay time.Duration) (*SignedClockClient, <-chan struct{}) {
	t.Helper()
	binding, pub, key, now := clockFixture(t)
	done := make(chan struct{})
	client, err := NewSignedClockClient(SignedClockClientOptions{Binding: binding, Audience: "pid1", PublicKey: pub, Dial: func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer close(done)
			defer b.Close()
			b.SetDeadline(time.Now().Add(2 * time.Second))
			var size [4]byte
			if _, err := io.ReadFull(b, size[:]); err != nil {
				return
			}
			request := make([]byte, binary.BigEndian.Uint32(size[:]))
			if _, err := io.ReadFull(b, request); err != nil {
				return
			}
			var r map[string]any
			if err := json.Unmarshal(request, &r); err != nil {
				return
			}
			r["utc"] = now.Format(time.RFC3339Nano)
			r["uncertainty"] = int64(time.Millisecond)
			if mutate != nil {
				mutate(r)
			}
			// Struct order is part of canonical signing format, independent of implementation helpers.
			bindingWire := struct {
				Namespace    string `json:"namespace"`
				AuthorityID  string `json:"authority_id"`
				Target       string `json:"target"`
				RestoreEpoch string `json:"restore_epoch"`
			}{binding.Namespace, binding.AuthorityID, binding.Target, binding.RestoreEpoch}
			if rb, ok := r["binding"].(map[string]any); ok {
				bindingWire.Target = rb["target"].(string)
			}
			claims := struct {
				Version     any `json:"version"`
				Purpose     any `json:"purpose"`
				Binding     any `json:"binding"`
				Audience    any `json:"audience"`
				Nonce       any `json:"nonce"`
				UTC         any `json:"utc"`
				Uncertainty any `json:"uncertainty"`
			}{r["version"], r["purpose"], bindingWire, r["audience"], r["nonce"], r["utc"], r["uncertainty"]}
			cb, _ := json.Marshal(claims)
			sig := ed25519.Sign(key, append([]byte("sandbox-authority-clock-observation:v1\x00"), cb...))
			wire, _ := json.Marshal(struct {
				Claims    any    `json:"claims"`
				Signature []byte `json:"signature"`
			}{claims, sig})
			if frame != nil {
				wire = frame(wire)
			}
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			binary.BigEndian.PutUint32(size[:], uint32(len(wire)))
			b.Write(size[:])
			b.Write(wire)
		}()
		return a, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return client, done
}
func TestSignedClockRejectsWire(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){"nonce": func(r map[string]any) { r["nonce"] = "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172" }, "audience": func(r map[string]any) { r["audience"] = "other" }, "binding": func(r map[string]any) { r["binding"].(map[string]any)["target"] = "other" }, "purpose": func(r map[string]any) { r["purpose"] = "operation_exec_start" }, "negative": func(r map[string]any) { r["uncertainty"] = -1 }, "oversecond": func(r map[string]any) { r["uncertainty"] = int64(time.Second + 1) }, "budget": func(r map[string]any) { r["uncertainty"] = int64(time.Second) }} {
		t.Run(name, func(t *testing.T) {
			c, done := clockPeer(t, mutate, nil, 0)
			if _, err := c.Observe(context.Background()); err == nil {
				t.Fatal("accepted invalid response")
			}
			waitClockPeer(t, done)
		})
	}
	for name, frame := range map[string]func([]byte) []byte{"duplicate": func(w []byte) []byte { return append([]byte(`{"signature":"AA==",`), w[1:]...) }, "unknown": func(w []byte) []byte { return append([]byte(`{"unknown":1,`), w[1:]...) }, "oversize": func(w []byte) []byte { return make([]byte, 4097) }, "truncated": func(w []byte) []byte { return w[:20] }, "signature": func(w []byte) []byte { w[len(w)-10] ^= 1; return w }} {
		t.Run(name, func(t *testing.T) {
			c, done := clockPeer(t, nil, frame, 0)
			if _, err := c.Observe(context.Background()); err == nil {
				t.Fatal("accepted malformed response")
			}
			waitClockPeer(t, done)
		})
	}
	c, done := clockPeer(t, nil, nil, 1100*time.Millisecond)
	if _, err := c.Observe(context.Background()); err == nil {
		t.Fatal("accepted late response")
	}
	waitClockPeer(t, done)
}
func TestSignedClockValidationAndCancel(t *testing.T) {
	b, p, k, now := clockFixture(t)
	var nilClock clockFunc
	for _, source := range []AuthorityClock{nil, nilClock} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := ServeSignedClock(context.Background(), l, SignedClockServerOptions{Binding: b, Key: k, Source: source}); err == nil {
			t.Fatal("accepted nil source")
		}
		l.Close()
	}
	opts := SignedClockClientOptions{Binding: b, Audience: "pid1", PublicKey: p, Dial: func(context.Context) (net.Conn, error) { return nil, nil }}
	for _, mutate := range []func(*SignedClockClientOptions){func(o *SignedClockClientOptions) { o.Dial = nil }, func(o *SignedClockClientOptions) { o.PublicKey = nil }, func(o *SignedClockClientOptions) { o.Audience = "" }, func(o *SignedClockClientOptions) { o.Binding.Target = "" }} {
		o := opts
		mutate(&o)
		if _, err := NewSignedClockClient(o); err == nil {
			t.Fatal("accepted invalid options")
		}
	}
	c, err := NewSignedClockClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Observe(context.Background()); err == nil {
		t.Fatal("accepted nil connection")
	}
	c, done := clockPeer(t, nil, nil, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Observe(ctx); err == nil {
		t.Fatal("accepted canceled call")
	}
	_ = done
	if _, err := c.Observe(nil); err == nil {
		t.Fatal("accepted nil context")
	}
	_ = now
}

func TestSignedClockIndependentPeerAndCancellation(t *testing.T) {
	c, done := clockPeer(t, nil, nil, 0)
	if _, err := c.Observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitClockPeer(t, done)
	c, done = clockPeer(t, nil, nil, 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.Observe(ctx); err == nil {
		t.Fatal("accepted canceled active read")
	}
	waitClockPeer(t, done)
	if time.Since(started) > time.Second {
		t.Fatal("cancellation did not bound read")
	}
}
func waitClockPeer(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("clock peer not joined")
	}
}
func TestClockFrames(t *testing.T) {
	for _, wire := range [][]byte{nil, {0}, {0, 0, 0, 0}, {0, 0, 16, 1}, {0, 0, 0, 4, 'a', 'b'}} {
		if _, err := readClockFrame(bytes.NewReader(wire)); err == nil {
			t.Fatal("accepted invalid length or truncation")
		}
	}
	var b bytes.Buffer
	if err := writeClockFrame(&b, bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatal(err)
	}
	if w, err := readClockFrame(&b); err != nil || len(w) != 4096 {
		t.Fatal("failed boundary frame")
	}
}
func TestClockSourceValidation(t *testing.T) {
	_, _, _, now := clockFixture(t)
	for name, o := range map[string]ClockObservation{"negative": {UTC: now, Uncertainty: -1}, "large": {UTC: now, Uncertainty: time.Second + 1}, "zero": {}, "offset": {UTC: now.In(time.FixedZone("offset", 0))}} {
		t.Run(name, func(t *testing.T) {
			if _, err := observeAuthorityClock(context.Background(), clockFunc(func(context.Context) (ClockObservation, error) { return o, nil })); err == nil {
				t.Fatal("accepted invalid source observation")
			}
		})
	}
	if _, err := observeAuthorityClock(context.Background(), clockFunc(func(ctx context.Context) (ClockObservation, error) {
		<-ctx.Done()
		return ClockObservation{UTC: now}, nil
	})); err == nil {
		t.Fatal("accepted source after deadline")
	}
}
func TestClockRequestStrictnessAndClientCopies(t *testing.T) {
	binding, pub, key, now := clockFixture(t)
	var calls atomic.Int32
	source := clockFunc(func(context.Context) (ClockObservation, error) { calls.Add(1); return ClockObservation{UTC: now}, nil })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeSignedClock(ctx, listener, SignedClockServerOptions{Binding: binding, Key: key, Source: source, MaxConnections: 1})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("server not joined")
		}
	}()
	opts := SignedClockClientOptions{Binding: binding, Audience: "pid1", PublicKey: append(ed25519.PublicKey(nil), pub...), Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}
	c, err := NewSignedClockClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.PublicKey[0] ^= 1
	opts.Binding.Target = "other"
	if _, err := c.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{[]byte(`{"version":1,"version":1}`), []byte(`{"unknown":1}`), bytes.Repeat([]byte(" "), 4097)} {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		var p [4]byte
		binary.BigEndian.PutUint32(p[:], uint32(len(wire)))
		conn.Write(p[:])
		conn.Write(wire)
		var out [1]byte
		if _, err := conn.Read(out[:]); err == nil {
			t.Fatal("invalid request received response")
		}
		conn.Close()
	}
	if calls.Load() != 1 {
		t.Fatal("malformed requests reached source")
	}
	bad := opts
	bad.PublicKey = append(ed25519.PublicKey(nil), pub...)
	bad.PublicKey[0] ^= 1
	bad.Binding = binding
	wrong, err := NewSignedClockClient(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Observe(ctx); err == nil {
		t.Fatal("wrong pinned key authenticated")
	}
}

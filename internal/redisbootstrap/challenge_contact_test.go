package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdentityContactVerified(t *testing.T) {
	r, _, _ := initialConfigFixture(t)
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	member := Member{DNS: r.Cluster.Members[0], Ordinal: 0}
	identity := VolumeIdentity{ClusterID: r.Cluster.ClusterID, Member: member, MarkerID: r.MarkerIDs[0], InitialConfig: Reserved}
	session, err := NewProofSession()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewIdentityHandler(r.Cluster, member, session, private, func(context.Context, ProofPurpose) (LocalObservation, error) {
		return LocalObservation{Volume: VolumeState{Identity: &identity}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	ch, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	proof, state, err := fetchIdentityProofContact(context.Background(), server.URL+"/v1/identity", r.Cluster, member, key, ch, &identity, nil)
	if err != nil || state != ContactVerified || proof.Session != session || !reflect.DeepEqual(proof.Observation.Volume.Identity, &identity) {
		t.Fatalf("valid signed member not verified: state=%v err=%v", state, err)
	}
}

func TestIdentityContactUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + listener.Addr().String() + "/v1/identity"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	c, keys, _ := initialConfigFixture(t)
	member := Member{DNS: c.Cluster.Members[0], Ordinal: 0}
	ch, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	_, state, err := fetchIdentityProofContact(context.Background(), url, c.Cluster, member, keys[0], ch, nil, nil)
	if err == nil || state != ContactUnreachable {
		t.Fatalf("connection failure not classified unreachable: state=%v err=%v", state, err)
	}
}

func TestIdentityContactReachableRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	c, keys, _ := initialConfigFixture(t)
	member := Member{DNS: c.Cluster.Members[0], Ordinal: 0}
	ch, err := NewIdentityChallenge(InventoryProof)
	if err != nil {
		t.Fatal(err)
	}
	_, state, err := fetchIdentityProofContact(context.Background(), server.URL+"/v1/identity", c.Cluster, member, keys[0], ch, nil, nil)
	if err == nil || state != ContactRejected {
		t.Fatalf("reachable unverified member treated unavailable: state=%v err=%v", state, err)
	}
}

func TestIdentityContactInvalidInputsDoNotConnect(t *testing.T) {
	pub, _, c, m, ch, observation := proofFixture(t, LiveProof)
	expected := *observation.Volume.Identity
	current := AuthenticatedEndpoint{Member: m, RunID: observation.RunID, Authenticated: true}
	type input struct {
		ctx       context.Context
		cluster   ClusterState
		member    Member
		key       ed25519.PublicKey
		challenge IdentityChallenge
		expected  *VolumeIdentity
		current   *AuthenticatedEndpoint
	}
	var calls atomic.Int32
	var connects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	for name, mutate := range map[string]func(*input){
		"nil context":           func(v *input) { v.ctx = nil },
		"cluster":               func(v *input) { v.cluster.Phase = "invalid" },
		"member":                func(v *input) { v.member.Ordinal = 3 },
		"key":                   func(v *input) { v.key = nil },
		"challenge":             func(v *input) { v.challenge.Nonce = "invalid" },
		"missing marker":        func(v *input) { v.expected = nil },
		"reserved live":         func(v *input) { id := *v.expected; id.InitialConfig = Reserved; v.expected = &id },
		"wrong expected member": func(v *input) { id := *v.expected; id.Member = Member{Ordinal: 1, DNS: c.Members[1]}; v.expected = &id },
		"missing current":       func(v *input) { v.current = nil },
		"unauthenticated":       func(v *input) { endpoint := *v.current; endpoint.Authenticated = false; v.current = &endpoint },
		"wrong current member": func(v *input) {
			endpoint := *v.current
			endpoint.Member = Member{Ordinal: 1, DNS: c.Members[1]}
			v.current = &endpoint
		},
		"invalid run id": func(v *input) { endpoint := *v.current; endpoint.RunID = "invalid"; v.current = &endpoint },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{ConnectStart: func(_, _ string) { connects.Add(1) }})
			v := input{ctx, c, m, pub, ch, &expected, &current}
			mutate(&v)
			proof, state, err := fetchIdentityProofContact(v.ctx, server.URL+"/v1/identity", v.cluster, v.member, v.key, v.challenge, v.expected, v.current)
			if err == nil || state != ContactRejected || !reflect.DeepEqual(proof, IdentityProof{}) || calls.Load() != 0 || connects.Load() != 0 {
				t.Fatalf("invalid input reached transport: state=%v calls=%d connects=%d err=%v", state, calls.Load(), connects.Load(), err)
			}
		})
	}
}

func TestIdentityContactRejectsReachableUntrustedProof(t *testing.T) {
	pub, private, c, m, ch, observation := proofFixture(t, InventoryProof)
	session, err := NewProofSession()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := SignIdentityProof(private, c, m, session, ch, observation)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*IdentityProof){
		"signature":    func(p *IdentityProof) { p.Signature = strings.Repeat("0", 128) },
		"nonce replay": func(p *IdentityProof) { p.Nonce = strings.Repeat("0", 64) },
		"marker": func(p *IdentityProof) {
			id := *p.Observation.Volume.Identity
			id.MarkerID = strings.Repeat("a", 32)
			p.Observation.Volume.Identity = &id
			p.Signature = hex.EncodeToString(ed25519.Sign(private, proofSigningBytes(*p)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			proof, state, err := fetchIdentityProofContact(context.Background(), server.URL+"/v1/identity", c, m, pub, ch, observation.Volume.Identity, nil)
			if err == nil || state != ContactRejected || !reflect.DeepEqual(proof, IdentityProof{}) || err.Error() != errIdentityTransport.Error() {
				t.Fatalf("untrusted reachable proof: state=%v err=%v", state, err)
			}
		})
	}
}

func TestIdentityContactCancellationIsNotUnreachable(t *testing.T) {
	r, keys, _ := initialConfigFixture(t)
	m := Member{DNS: r.Cluster.Members[0], Ordinal: 0}
	ch, _ := NewIdentityChallenge(InventoryProof)
	for _, alreadyCancelled := range []bool{true, false} {
		t.Run(fmt.Sprint(alreadyCancelled), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { close(entered); <-release }))
			defer func() { close(release); server.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if alreadyCancelled {
				cancel()
			} else {
				go func() { <-entered; cancel() }()
			}
			proof, state, err := fetchIdentityProofContact(ctx, server.URL+"/v1/identity", r.Cluster, m, keys[0], ch, nil, nil)
			if !errors.Is(err, context.Canceled) || state != ContactRejected || !reflect.DeepEqual(proof, IdentityProof{}) {
				t.Fatalf("cancellation classified offline: state=%v err=%v", state, err)
			}
		})
	}
}

func TestIdentityContactConnectedHeaderTimeoutRejected(t *testing.T) {
	r, keys, _ := initialConfigFixture(t)
	m := Member{DNS: r.Cluster.Members[0], Ordinal: 0}
	ch, _ := NewIdentityChallenge(InventoryProof)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { close(entered); <-release }))
	defer func() { close(release); server.Close() }()
	started := time.Now()
	proof, state, err := fetchIdentityProofContact(context.Background(), server.URL+"/v1/identity", r.Cluster, m, keys[0], ch, nil, nil)
	select {
	case <-entered:
	default:
		t.Fatal("did not establish actual connection")
	}
	if err == nil || err.Error() != errIdentityTransport.Error() || state != ContactRejected || !reflect.DeepEqual(proof, IdentityProof{}) || time.Since(started) > 4*time.Second {
		t.Fatalf("connected timeout classified offline: state=%v err=%v", state, err)
	}
}

func TestIdentityContactIncompleteDialIsNotUnreachable(t *testing.T) {
	r, keys, _ := initialConfigFixture(t)
	m := Member{DNS: r.Cluster.Members[0], Ordinal: 0}
	ch, _ := NewIdentityChallenge(InventoryProof)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	release := make(chan struct{})
	connected := make(chan struct{})
	returned := make(chan struct{})
	defer func() {
		close(release)
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Error("dial trace did not return")
		}
		server.Close()
	}()
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{ConnectDone: func(_, _ string, err error) {
		if err == nil {
			close(connected)
			<-release
			close(returned)
		}
	}})
	proof, state, err := fetchIdentityProofContact(ctx, server.URL+"/v1/identity", r.Cluster, m, keys[0], ch, nil, nil)
	select {
	case <-connected:
	default:
		t.Fatal("did not physically establish TCP")
	}
	if err == nil || state != ContactRejected || !reflect.DeepEqual(proof, IdentityProof{}) {
		t.Fatalf("incomplete asynchronous dial classified offline: state=%v err=%v", state, err)
	}
}

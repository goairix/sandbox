package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"net"
	"net/http"
	"sync/atomic"
)

// IdentityContactState describes this request only, not global network health.
// Rejected includes any reachable but unverifiable member and must not be
// ignored as an offline vote by a recovery caller.
type IdentityContactState int

const (
	ContactRejected IdentityContactState = iota
	ContactUnreachable
	ContactVerified
)

// FetchIdentityProofContact uses the same fixed endpoint and strict proof
// verification as FetchIdentityProof. It grants no start or election authority.
func FetchIdentityProofContact(ctx context.Context, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, current *AuthenticatedEndpoint) (IdentityProof, IdentityContactState, error) {
	return fetchIdentityProofContact(ctx, "http://"+m.DNS+":18080/v1/identity", c, m, key, ch, expected, current)
}

func fetchIdentityProofContact(ctx context.Context, url string, c ClusterState, m Member, key ed25519.PublicKey, ch IdentityChallenge, expected *VolumeIdentity, current *AuthenticatedEndpoint) (IdentityProof, IdentityContactState, error) {
	if ctx == nil || m.Validate(c) != nil || !validProofPublicKey(key) || validateChallenge(ch) != nil {
		return IdentityProof{}, ContactRejected, errIdentityTransport
	}
	if err := ctx.Err(); err != nil {
		return IdentityProof{}, ContactRejected, err
	}
	if expected != nil && (expected.Validate(c) != nil || expected.Member != m) {
		return IdentityProof{}, ContactRejected, errIdentityTransport
	}
	if current != nil && (!current.Authenticated || current.Member != m || !runIDPattern.MatchString(current.RunID)) {
		return IdentityProof{}, ContactRejected, errIdentityTransport
	}
	if ch.Purpose == LiveProof && (expected == nil || expected.InitialConfig != Configured || current == nil) {
		return IdentityProof{}, ContactRejected, errIdentityTransport
	}
	client := newIdentityHTTPClient()
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	dial := transport.DialContext
	var attempted, connected, failed atomic.Bool
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		attempted.Store(true)
		conn, err := dial(ctx, network, address)
		if err == nil && conn != nil {
			connected.Store(true)
		} else if err != nil && conn == nil {
			// Transport may return on its request timeout while DialContext is
			// still running. Absence of success does not prove a failed dial.
			failed.Store(true)
		}
		return conn, err
	}
	proof, err := fetchIdentityProof(ctx, client, url, c, m, key, ch, expected, current)
	// Caller cancellation never means that the member was unreachable; nor
	// may verified bytes obtained after cancellation authorize a continuation.
	if cause := ctx.Err(); cause != nil {
		return IdentityProof{}, ContactRejected, cause
	}
	if err == nil {
		return proof, ContactVerified, nil
	}
	if attempted.Load() && failed.Load() && !connected.Load() {
		return IdentityProof{}, ContactUnreachable, errIdentityTransport
	}
	return IdentityProof{}, ContactRejected, errIdentityTransport
}

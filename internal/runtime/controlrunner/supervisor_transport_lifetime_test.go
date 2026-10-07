package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	j "github.com/goairix/sandbox/internal/runtime/controltarget"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func shutdownHistory(t *testing.T) (*j.Journal, string) {
	t.Helper()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuerPub, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binding := p.TrustBinding{Namespace: "/sandbox/shutdown/v1/", AuthorityID: "test", Target: "test", RestoreEpoch: "epoch"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
	require.NoError(t, err)
	now := time.Now().UTC()
	cert, err := p.SignCommandIssuerCertificate(rootKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerPub, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	issuer, err := verifier.VerifyCommandIssuerCertificate(cert, now)
	require.NoError(t, err)
	identity := j.JournalIdentity{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, SandboxID: "history", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: uuid.NewString()}}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(directory, 0700))
	journal, err := j.CreateClosedJournal(context.Background(), j.JournalOptions{Directory: filepath.Join(directory, "journal"), Identity: identity, DataGateEpoch: 1, ManagementUID: uint32(os.Geteuid())})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	descriptor, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/never-run"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 1})
	require.NoError(t, err)
	c := p.ExecStartContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "history", OperationDigest: strings.Repeat("b", 64), SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: 1, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 1, LeaseID: 1, Runtime: identity.Runtime, ExpiresAt: now.Add(time.Minute)}
	ticket, err := p.SignExecStartTicket(issuerKey, issuer, p.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-time.Second), NotAfter: now.Add(10 * time.Second)})
	require.NoError(t, err)
	evidence, err := verifier.VerifyExecStartTicket(ticket, cert, c, descriptor, now)
	require.NoError(t, err)
	_, err = journal.RecordUnknown(context.Background(), evidence)
	require.NoError(t, err)
	return journal, c.CommandID
}
func shutdownAwait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("owned shutdown worker did not join")
	}
}

// This is a resource-lifetime proof, not a fake PID1/activation or user result.
// Passive history is written through the public verified-evidence API. The
// test-only scheduling pause occurs after actual Lookup releases its mutex.
func TestTransportShutdownHistoryBorrow(t *testing.T) {
	journal, id := shutdownHistory(t)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var life transportLifetime
	conn, peer := net.Pipe()
	defer peer.Close()
	borrow, _ := life.borrow(context.Background(), conn)
	require.NotNil(t, borrow)
	looked, release, workerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	signatureOK := make(chan bool, 1)
	go func() {
		defer close(workerDone)
		defer borrow.release()
		record, err := journal.Lookup(context.Background(), id)
		if err != nil || record == nil {
			signatureOK <- false
			close(looked)
			return
		}
		close(looked)
		<-release
		signatureOK <- ed25519.Verify(public, []byte(record.TicketDigest), ed25519.Sign(key, []byte(record.TicketDigest)))
	}()
	shutdownAwait(t, looked)
	stopped, closed, destroyed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stopErr, destroyErr := errors.New("stop retained"), errors.New("destroy retained")
	closeResult := make(chan error, 2)
	go func() {
		closeResult <- life.close(func() (bool, error) { close(stopped); return true, stopErr }, func() error {
			clear(key)
			err := errors.Join(journal.Close(), destroyErr)
			close(destroyed)
			return err
		})
		close(closed)
	}()
	shutdownAwait(t, stopped)
	// Concurrent Close is a join, not an early successful return.
	second := make(chan struct{})
	go func() {
		closeResult <- life.close(func() (bool, error) { t.Error("second stop owner"); return true, nil }, func() error { t.Error("second destroy owner"); return nil })
		close(second)
	}()
	lateConn, latePeer := net.Pipe()
	defer lateConn.Close()
	defer latePeer.Close()
	late, _ := life.borrow(context.Background(), lateConn)
	if late != nil {
		late.release()
		t.Error("registration accepted after closing")
	}
	select {
	case <-destroyed:
		t.Error("resources destroyed while history query paused")
	case <-time.After(50 * time.Millisecond):
	}
	if journal.Status().Closed {
		t.Error("journal destroyed before history borrower joined")
	}
	select {
	case <-second:
		t.Error("concurrent Close returned before borrower joined")
	default:
	}
	close(release)
	shutdownAwait(t, workerDone)
	shutdownAwait(t, closed)
	shutdownAwait(t, second)
	if !<-signatureOK {
		t.Error("history signed after original key destruction")
	}
	for range 2 {
		err := <-closeResult
		require.ErrorIs(t, err, stopErr)
		require.ErrorIs(t, err, destroyErr)
	}
	require.True(t, journal.Status().Closed)
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(key))
}

func TestTransportShutdownActiveStream(t *testing.T) {
	var life transportLifetime
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.True(t, life.listen(listener, cancel))
	conn, peer := net.Pipe()
	defer peer.Close()
	borrow, ctx := life.borrow(parent, conn)
	require.NotNil(t, borrow)
	output := make(chan streamFrame, 2)
	executionStopped, ownerDone := make(chan struct{}), make(chan struct{})
	var drained atomic.Int32
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		defer borrow.release()
		for range output {
			drained.Add(1)
		}
		<-ownerDone
	}()
	ownerJoined := make(chan struct{})
	go func() {
		defer close(ownerJoined)
		<-executionStopped
		for range 4 {
			output <- streamFrame{kind: monitorStdout, data: []byte("bounded")}
		}
		close(output)
		close(ownerDone)
	}()
	result := make(chan error, 1)
	go func() {
		result <- life.close(func() (bool, error) {
			if ctx.Err() == nil || parent.Err() == nil {
				t.Error("borrower context not canceled before execution stop")
			}
			close(executionStopped)
			<-ownerDone
			return true, nil
		}, func() error {
			if drained.Load() != 4 {
				t.Error("destroyed before stream drain")
			}
			return nil
		})
	}()
	shutdownAwait(t, workerDone)
	shutdownAwait(t, ownerJoined)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown waited for stream before stopping its execution")
	}
	_, err = listener.Accept()
	require.ErrorIs(t, err, net.ErrClosed)
	require.False(t, life.listen(listener, cancel))
}

// Failure to join an execution leaves signer/journal intact for the existing
// isolation path; shutdown must never claim successful resource destruction.
func TestTransportShutdownUnjoinedExecutionRetainsResources(t *testing.T) {
	var life transportLifetime
	conn, peer := net.Pipe()
	defer peer.Close()
	borrow, _ := life.borrow(context.Background(), conn)
	require.NotNil(t, borrow)
	defer borrow.release()
	var destroyed bool
	cause := errors.New("execution join deadline")
	err := life.close(func() (bool, error) { return false, cause }, func() error { destroyed = true; return nil })
	require.ErrorIs(t, err, cause)
	require.False(t, destroyed)
	err = life.close(func() (bool, error) { t.Fatal("second stop owner"); return true, nil }, func() error { t.Fatal("second destroy owner"); return nil })
	require.ErrorIs(t, err, cause)
}

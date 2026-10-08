package etcd

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type archiveConsumerClock struct{ now time.Time }

func (c archiveConsumerClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: c.now}, ctx.Err()
}

type archiveCloseControl struct {
	entered, release chan struct{}
	once             sync.Once
}
type archiveCloseConn struct {
	net.Conn
	control *archiveCloseControl
}

func (c *archiveCloseConn) Close() error {
	c.control.once.Do(func() { close(c.control.entered) })
	<-c.control.release
	return c.Conn.Close()
}

// This genuinely signed mTLS consumer fixture is not PID1 or current KV proof.
func archiveConsumerFixture(t *testing.T, data []byte, entries uint64, fault string, closeControl ...*archiveCloseControl) (taskArchiveExpected, <-chan error) {
	t.Helper()
	now := time.Now().UTC()
	rootPublic, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, runtimeKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	b := p.TrustBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}
	v, err := p.NewManagementVerifier(b, []ed25519.PublicKey{rootPublic})
	require.NoError(t, err)
	id := p.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: uuid.NewString()}}
	iw, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)})
	require.NoError(t, err)
	rw, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, PublicKey: runtimeKey.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), SandboxID: id.SandboxID, WorkspaceHash: id.WorkspaceHash, Generation: id.Generation, Runtime: id.Runtime})
	require.NoError(t, err)
	issuer, err := v.VerifyCommandIssuerCertificate(iw, now)
	require.NoError(t, err)
	old := p.TaskCloseDataContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("b", 64), ClaimID: uuid.NewString(), WorkerID: "worker", SandboxID: id.SandboxID, WorkspaceHash: id.WorkspaceHash, Generation: id.Generation, DataGateEpoch: 3, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: id.Runtime}
	q := old
	q.CommandID = uuid.NewString()
	current := old
	current.CommandID = uuid.NewString()
	c := p.TaskWorkspaceArchiveContext{Current: current, QuiescenceContext: p.TaskUserQuiescenceContext{Current: q, CloseDataContext: old, CloseDataTicketDigest: strings.Repeat("c", 64), CloseDataReceiptDigest: strings.Repeat("d", 64), CloseDataIntentRevision: 12}, QuiescenceTicketDigest: strings.Repeat("e", 64), QuiescenceReceiptDigest: strings.Repeat("f", 64), PlanDigest: strings.Repeat("a", 64), QuiescenceIntentRevision: 13, FinalSyncRevision: 14}
	before, after, sessionAfter := now.Add(-2*time.Second), now.Add(20*time.Second), now.Add(40*time.Second)
	ticket, err := p.SignTaskWorkspaceArchiveTicket(issuerKey, issuer, p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: c, NotBefore: before, NotAfter: after, SessionNotAfter: sessionAfter})
	require.NoError(t, err)
	hash := sha256.Sum256(ticket)
	digest := hex.EncodeToString(hash[:])
	sum := sha256.Sum256(data)
	claims := p.TaskWorkspaceArchiveReceiptClaims{Version: 1, State: "archive_complete", Context: c, TicketDigest: digest, NotBefore: before, NotAfter: after, SessionNotAfter: sessionAfter, ArchiveBytes: uint64(len(data)), EntryCount: entries, SHA256: hex.EncodeToString(sum[:])}
	switch fault {
	case "hash":
		claims.SHA256 = strings.Repeat("0", 64)
	case "count":
		claims.EntryCount++
	case "bytes":
		claims.ArchiveBytes += 512
	case "attribution":
		claims.Context.PlanDigest = strings.Repeat("c", 64)
	case "window":
		claims.SessionNotAfter = claims.SessionNotAfter.Add(time.Second)
	}
	signer := runtimeKey
	if fault == "signature" {
		signer = issuerKey
	}
	receipt, err := p.SignTaskWorkspaceArchiveReceipt(signer, claims)
	require.NoError(t, err)
	ic, err := p.NewManagementTLSCredential(issuerKey, iw, "command_issuer")
	require.NoError(t, err)
	rc, err := p.NewManagementTLSCredential(runtimeKey, rw, "runtime_receipt")
	require.NoError(t, err)
	clock := archiveConsumerClock{now}
	server, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: v, Clock: clock, Credential: rc, PeerCertificate: iw, Server: true})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	d, err := tr.NewDestination(tr.DestinationOptions{Identity: id, RuntimeCertificate: rw, Credential: ic, Verifier: v, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		raw, err := dial.DialContext(ctx, "tcp", listener.Addr().String())
		if err == nil && len(closeControl) != 0 {
			return &archiveCloseConn{Conn: raw, control: closeControl[0]}, nil
		}
		return raw, err
	}})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Server(raw, server)
		if err = conn.Handshake(); err != nil {
			done <- err
			return
		}
		request, err := tr.ReadRuntimeRequest(conn)
		if err != nil || request.Archive == nil {
			done <- errors.Join(err, errTaskArchive)
			return
		}
		if fault == "partial-frame" {
			var header [5]byte
			header[0] = tr.ArchiveData
			binary.BigEndian.PutUint32(header[1:], 10)
			_, err = conn.Write(append(header[:], byte(1)))
			if err == nil {
				err = conn.CloseWrite()
			}
			done <- err
			return
		}
		if fault != "no-data" {
			wire := data
			if fault == "truncated-frame" {
				wire = wire[:len(wire)-1]
			}
			for len(wire) > 0 {
				n := len(wire)
				if n > 32768 {
					n = 32768
				}
				if err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveData, wire[:n]); err != nil {
					done <- err
					return
				}
				wire = wire[n:]
			}
		}
		if fault != "no-complete" {
			if err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveComplete, receipt); err != nil {
				done <- err
				return
			}
		}
		if fault == "trailing" {
			_, err = conn.Write([]byte{1})
		}
		if err == nil && fault != "raw-eof" {
			err = conn.CloseWrite()
		}
		done <- err
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("consumer TLS server did not join")
		}
	})
	e := taskArchiveExpected{verifier: v, destination: d, envelope: tr.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive", Context: c, TicketDigest: digest, Ticket: ticket, IssuerCertificate: iw, OriginalQuiescenceReceipt: []byte("fixture historical prerequisite")}, context: c, digest: digest, before: before, after: after, sessionAfter: sessionAfter, now: func(ctx context.Context) (time.Time, error) { return now, ctx.Err() }}
	return e, done
}

func archiveTarBytes(t *testing.T, headers []*tar.Header, bodies []string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i, h := range headers {
		require.NoError(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(bodies[i]))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return b.Bytes()
}

func archiveValidTar(t *testing.T) []byte {
	return archiveTarBytes(t, []*tar.Header{
		{Name: "workspace", Typeflag: tar.TypeDir, Mode: 0700, ModTime: time.Unix(1, 123).UTC(), Format: tar.FormatPAX},
		{Name: "workspace/z", Typeflag: tar.TypeDir, Mode: 0700, ModTime: time.Unix(1, 123).UTC(), Format: tar.FormatPAX},
		{Name: "workspace/z/" + strings.Repeat("x", 120), Typeflag: tar.TypeReg, Mode: 0600, Size: 5, ModTime: time.Unix(1, 123).UTC(), Format: tar.FormatPAX},
		{Name: "workspace/a", Typeflag: tar.TypeReg, Mode: 0600, Size: 3, Format: tar.FormatPAX},
	}, []string{"", "", "hello", "bye"})
}

func TestTaskArchiveReceiveRealTLSOriginalAndCopiedWitness(t *testing.T) {
	data := archiveValidTar(t)
	e, done := archiveConsumerFixture(t, data, 4, "")
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	defer s.close()
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w, err := r.receive(ctx, e)
	require.NoError(t, err)
	require.NotNil(t, w)
	d, err := w.inspect()
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.Equal(t, uint64(len(data)), d.Bytes)
	require.Equal(t, uint64(4), d.Entries)
	require.Equal(t, hex.EncodeToString(sum[:]), d.SHA256)
	actual := make([]byte, len(data))
	_, err = r.file.ReadAt(actual, 0)
	require.NoError(t, err)
	require.Equal(t, data, actual)
	d.SHA256 = "changed"
	original, err := w.inspect()
	require.NoError(t, err)
	require.NotEqual(t, d.SHA256, original.SHA256)
	copyWitness := *w
	_, err = copyWitness.inspect()
	require.Error(t, err)
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	_, err = w.inspect()
	require.Error(t, err, "sealed store refuses witness before individual cleanup")
	s.mu.Lock()
	s.closed = false
	s.mu.Unlock()
	_, err = (&taskReceivedArchive{}).inspect()
	require.Error(t, err)
	_, err = r.receive(ctx, e)
	require.Error(t, err)
	require.NoError(t, <-done)
	require.NoError(t, r.close())
	_, err = w.inspect()
	require.Error(t, err)
}

func TestTaskArchiveReceiveRefusesSignedAndTransportFaults(t *testing.T) {
	for _, fault := range []string{"hash", "count", "bytes", "attribution", "window", "signature", "no-data", "trailing", "raw-eof", "no-complete", "truncated-frame", "partial-frame"} {
		t.Run(fault, func(t *testing.T) {
			data := archiveValidTar(t)
			e, _ := archiveConsumerFixture(t, data, 4, fault)
			s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
			require.NoError(t, err)
			defer s.close()
			r, err := s.reserve(context.Background())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			w, err := r.receive(ctx, e)
			require.Error(t, err)
			require.Nil(t, w)
			require.Nil(t, r.witness)
			require.Equal(t, uint64(1), s.jobs)
			require.Equal(t, s.limits.JobBytes, s.disk)
		})
	}
}

func TestTaskArchiveReceiveJobDATAExhaustion(t *testing.T) {
	data := archiveValidTar(t)
	e, _ := archiveConsumerFixture(t, data, 4, "")
	l := archiveTestLimits()
	l.JobBytes = 1536
	l.TotalBytes = 3072
	s, err := newTaskArchiveStore(archiveTestDirectory(t), l)
	require.NoError(t, err)
	defer s.close()
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w, err := r.receive(ctx, e)
	require.Error(t, err)
	require.Nil(t, w)
	require.Nil(t, r.witness)
	info, err := r.file.Stat()
	require.NoError(t, err)
	require.LessOrEqual(t, info.Size(), int64(l.JobBytes))
	require.Equal(t, l.JobBytes, s.disk)
	require.Equal(t, uint64(1), s.jobs)
}

func TestTaskArchiveReceiveCloseJoinsPhysicalSessionClose(t *testing.T) {
	control := &archiveCloseControl{entered: make(chan struct{}), release: make(chan struct{})}
	e, _ := archiveConsumerFixture(t, archiveValidTar(t), 4, "", control)
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	received := make(chan error, 1)
	go func() {
		w, err := r.receive(context.Background(), e)
		if w != nil {
			received <- errTaskArchive
		} else {
			received <- err
		}
	}()
	<-control.entered
	closed := make(chan error, 1)
	go func() { closed <- r.close() }()
	<-r.ctx.Done()
	select {
	case <-closed:
		t.Fatal("refunded before original transport Close returned")
	case <-time.After(20 * time.Millisecond):
	}
	s.mu.Lock()
	held := s.jobs == 1 && s.disk == s.limits.JobBytes
	s.mu.Unlock()
	require.True(t, held)
	_, err = r.file.Stat()
	require.NoError(t, err)
	close(control.release)
	require.Error(t, <-received)
	require.NoError(t, <-closed)
	require.Zero(t, s.jobs)
	require.Nil(t, r.witness)
	require.NoError(t, s.close())
}

func TestTaskArchiveReceivePAXLargeSizeHeader(t *testing.T) {
	// Exercise genuine Go1.25.6 FormatPAX output above octal's 33-bit bound,
	// without allocating or writing the declared regular body.
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "workspace/huge", Typeflag: tar.TypeReg, Size: 1 << 33, Format: tar.FormatPAX}))
	raw := b.Bytes()
	require.Equal(t, byte(tar.TypeXHeader), raw[156])
	n, err := strconv.ParseUint(strings.Trim(string(raw[124:136]), " \x00"), 8, 64)
	require.NoError(t, err)
	size, err := taskArchivePAX(raw[512 : 512+int(n)])
	require.NoError(t, err)
	require.NotNil(t, size)
	require.Equal(t, uint64(1<<33), *size)
	regular := 512 + ((int(n) + 511) &^ 511)
	require.Equal(t, "00000000000\x00", string(raw[regular+124:regular+136]))
}

func TestTaskArchiveReceiveRejectsExpectedBindingAndCanceled(t *testing.T) {
	for _, fault := range []string{"query", "digest", "context", "before", "after", "session", "late", "cancel", "copied-destination", "copied-spool"} {
		t.Run(fault, func(t *testing.T) {
			e, _ := archiveConsumerFixture(t, archiveValidTar(t), 4, "")
			s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
			require.NoError(t, err)
			defer s.close()
			r, err := s.reserve(context.Background())
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch fault {
			case "query":
				e.envelope.Purpose = "task_workspace_archive_query"
			case "digest":
				e.digest = strings.Repeat("0", 64)
			case "context":
				e.context.PlanDigest = strings.Repeat("0", 64)
			case "before":
				e.before = e.before.Add(time.Nanosecond)
			case "after":
				e.after = e.after.Add(time.Nanosecond)
			case "session":
				e.sessionAfter = e.sessionAfter.Add(time.Nanosecond)
			case "late":
				e.now = func(context.Context) (time.Time, error) { return e.sessionAfter, nil }
			case "cancel":
				cancel()
			case "copied-destination":
				copyD := *e.destination
				e.destination = &copyD
			case "copied-spool":
				copyR := *r
				r = &copyR
			}
			w, err := r.receive(ctx, e)
			require.Error(t, err)
			require.Nil(t, w)
		})
	}
}

func TestTaskArchiveReceiveCloseJoinsActualCallback(t *testing.T) {
	e, _ := archiveConsumerFixture(t, archiveValidTar(t), 4, "")
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	originalNow := e.now
	e.now = func(ctx context.Context) (time.Time, error) { close(entered); <-release; return originalNow(ctx) }
	received := make(chan error, 1)
	go func() { _, err := r.receive(context.Background(), e); received <- err }()
	<-entered
	var wg sync.WaitGroup
	closed := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); closed <- r.close() }()
	}
	<-r.ctx.Done()
	select {
	case <-closed:
		t.Fatal("close returned before arbitrary callback")
	case <-time.After(20 * time.Millisecond):
	}
	s.mu.Lock()
	held := s.jobs == 1 && s.disk == s.limits.JobBytes
	s.mu.Unlock()
	require.True(t, held)
	close(release)
	require.Error(t, <-received)
	wg.Wait()
	require.NoError(t, <-closed)
	require.NoError(t, <-closed)
	require.Zero(t, s.jobs)
	require.NoError(t, s.close())
}

func TestTaskArchiveReceiveTarConstraintsActualFile(t *testing.T) {
	root := func() *tar.Header {
		return &tar.Header{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0700, Format: tar.FormatPAX}
	}
	for _, fault := range []string{"first", "dot", "dotdot", "absolute", "backslash", "control", "utf8", "depth", "path", "duplicate", "missing-parent", "file-parent", "symlink", "hardlink", "owner", "group", "uname", "mode", "gnu", "sparse", "global-pax", "unsupported-pax", "dir-size", "truncated", "trailing", "end", "size", "checksum", "entry-limit"} {
		t.Run(fault, func(t *testing.T) {
			headers := []*tar.Header{root(), {Name: "workspace/a", Typeflag: tar.TypeReg, Mode: 0600, Size: 1, Format: tar.FormatPAX}}
			bodies := []string{"", "x"}
			h := headers[1]
			switch fault {
			case "first":
				headers[0].Name = "elsewhere/"
			case "dot":
				h.Name = "workspace/./a"
			case "dotdot":
				h.Name = "workspace/../a"
			case "absolute":
				h.Name = "/workspace/a"
			case "backslash":
				h.Name = "workspace/a\\b"
			case "control":
				h.Name = "workspace/a\nb"
			case "utf8":
				h.Name = "workspace/\xff"
			case "depth":
				h.Name = "workspace/" + strings.Repeat("a/", 128) + "x"
			case "path":
				h.Name = "workspace/" + strings.Repeat("a", 4096)
			case "duplicate":
				headers = append(headers, h)
				bodies = append(bodies, "x")
			case "missing-parent":
				h.Name = "workspace/missing/a"
			case "file-parent":
				headers = append(headers, &tar.Header{Name: "workspace/a/x", Typeflag: tar.TypeReg, Size: 1, Format: tar.FormatPAX})
				bodies = append(bodies, "x")
			case "symlink":
				h.Typeflag = tar.TypeSymlink
				h.Linkname = "a"
				h.Size = 0
			case "hardlink":
				h.Typeflag = tar.TypeLink
				h.Linkname = "a"
				h.Size = 0
			case "owner":
				h.Uid = 1
			case "group":
				h.Gid = 1
			case "uname":
				h.Uname = "user"
			case "mode":
				h.Mode = 04700
			case "gnu":
				h.Format = tar.FormatGNU
			case "unsupported-pax":
				h.PAXRecords = map[string]string{"comment": "bad"}
			}
			// Invalid UTF8 needs physical mutation because tar.Writer refuses it.
			if fault == "utf8" {
				h.Name = "workspace/a"
			}
			data := archiveTarBytes(t, headers, bodies)
			switch fault {
			case "sparse":
				data[512+156] = tar.TypeGNUSparse
			case "global-pax":
				data[512+156] = tar.TypeXGlobalHeader
			case "dir-size":
				copy(data[124:136], []byte("00000000001\x00"))
			case "truncated":
				data = data[:len(data)-512]
			case "trailing":
				data = append(data, make([]byte, 512)...)
			case "end":
				data[len(data)-512] = 1
			case "size":
				copy(data[512+124:512+136], []byte("77777777777\x00"))
			case "checksum":
				data[10] ^= 1
			case "utf8":
				data[512+10] = 255
			}
			f, err := os.CreateTemp(archiveTestDirectory(t), "tar-")
			require.NoError(t, err)
			defer f.Close()
			_, err = f.Write(data)
			require.NoError(t, err)
			limit := uint64(32)
			if fault == "entry-limit" {
				limit = 1
			}
			_, err = taskArchiveValidateTar(context.Background(), f, uint64(len(data)), limit, make([]byte, 64*33))
			require.Error(t, err)
		})
	}
	data := archiveValidTar(t)
	f, err := os.CreateTemp(archiveTestDirectory(t), "tar-")
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(data)
	require.NoError(t, err)
	count, err := taskArchiveValidateTar(context.Background(), f, uint64(len(data)), 32, make([]byte, 64*33))
	require.NoError(t, err)
	require.Equal(t, uint64(4), count)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = taskArchiveValidateTar(ctx, f, uint64(len(data)), 32, make([]byte, 64*33))
	require.ErrorIs(t, err, context.Canceled)
	_, err = f.ReadAt(make([]byte, 1), int64(len(data)))
	require.ErrorIs(t, err, io.EOF)
}

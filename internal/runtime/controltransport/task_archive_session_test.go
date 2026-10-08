package controltransport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

// Use the real mutually pinned TLS configuration. The server exclusively owns
// its connection, and cleanup physically joins it even after a failed assert.
func archiveSessionPair(t *testing.T, serve func(*tls.Conn, net.Conn, RuntimeRequest) error) (*Session, <-chan error) {
	t.Helper()
	o, server, _, _, _, _ := destinationFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	o.Dial = func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(o)
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
		if err = raw.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			done <- err
			return
		}
		conn := tls.Server(raw, server)
		var request RuntimeRequest
		if err = conn.Handshake(); err == nil {
			request, err = ReadRuntimeRequest(conn)
		}
		if err == nil {
			err = serve(conn, raw, request)
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		s.Close()
		listener.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("archive TLS server did not physically join")
		}
	})
	return s, done
}

func TestTaskWorkspaceArchiveSessionIndependentAdmissionAndModes(t *testing.T) {
	release := make(chan struct{})
	s, done := archiveSessionPair(t, func(conn *tls.Conn, _ net.Conn, r RuntimeRequest) error {
		if r.Archive == nil || r.Archive.Purpose != "task_workspace_archive" || r.Quiesce != nil || r.CloseData != nil {
			return ErrFrame
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			return errors.New("response release timed out")
		}
		if err := WriteTaskWorkspaceArchiveFrame(conn, ArchiveData, []byte("tar bytes")); err != nil {
			return err
		}
		if err := WriteTaskWorkspaceArchiveFrame(conn, ArchiveComplete, []byte("receipt")); err != nil {
			return err
		}
		return conn.CloseWrite()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := s.ReadTaskWorkspaceArchiveFrame(ctx)
	require.ErrorIs(t, err, ErrFrame, "archive response before send")
	require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(ctx), ErrFrame)
	admission, cancelAdmission := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelAdmission()
	require.NoError(t, s.SendTaskWorkspaceArchive(admission, archiveEnvelope()))
	<-admission.Done()
	close(release)
	_, _, err = s.Read(ctx)
	require.ErrorIs(t, err, ErrFrame, "generic response cannot consume archive bytes")
	require.ErrorIs(t, s.Send(ctx, frameRequest(t)), ErrFrame)
	require.ErrorIs(t, s.SendTaskClose(ctx, taskFrame()), ErrFrame)
	require.ErrorIs(t, s.SendTaskQuiesce(ctx, quiesceFrame()), ErrFrame)
	require.ErrorIs(t, s.SendTaskWorkspaceArchive(ctx, archiveEnvelope()), ErrFrame)
	require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(ctx), ErrFrame, "DATA cannot stand in for completion")
	kind, wire, err := s.ReadTaskWorkspaceArchiveFrame(ctx)
	require.NoError(t, err, "response outlives expired admission context")
	require.Equal(t, ArchiveData, kind)
	require.Equal(t, "tar bytes", string(wire))
	kind, wire, err = s.ReadTaskWorkspaceArchiveFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, ArchiveComplete, kind)
	require.Equal(t, "receipt", string(wire))
	_, _, err = s.ReadTaskWorkspaceArchiveFrame(ctx)
	require.ErrorIs(t, err, ErrFrame, "completion transitions to explicit EOF check")
	require.NoError(t, s.RequireTaskWorkspaceArchiveEOF(ctx))
	require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(ctx), ErrFrame, "EOF proof is consumed once")
	require.NoError(t, <-done)
}

func TestTaskWorkspaceArchiveSessionRejectsTrailingAndAbruptEOF(t *testing.T) {
	for _, ending := range []string{"tls-close-notify", "raw-close", "trailing-byte", "trailing-data", "trailing-complete"} {
		t.Run(ending, func(t *testing.T) {
			s, done := archiveSessionPair(t, func(conn *tls.Conn, raw net.Conn, r RuntimeRequest) error {
				if r.Archive == nil {
					return ErrFrame
				}
				if err := WriteTaskWorkspaceArchiveFrame(conn, ArchiveComplete, []byte("receipt")); err != nil {
					return err
				}
				switch ending {
				case "raw-close":
					return raw.Close()
				case "trailing-byte":
					if _, err := conn.Write([]byte{0}); err != nil {
						return err
					}
				case "trailing-data", "trailing-complete":
					kind := ArchiveData
					if ending == "trailing-complete" {
						kind = ArchiveComplete
					}
					if err := WriteTaskWorkspaceArchiveFrame(conn, kind, []byte("extra")); err != nil {
						return err
					}
				}
				return conn.CloseWrite()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			e := archiveEnvelope()
			e.Purpose = "task_workspace_archive_query"
			e.Ticket, e.IssuerCertificate, e.OriginalQuiescenceReceipt = nil, nil, nil
			require.NoError(t, s.SendTaskWorkspaceArchive(ctx, e))
			kind, _, err := s.ReadTaskWorkspaceArchiveFrame(ctx)
			require.NoError(t, err)
			require.Equal(t, ArchiveComplete, kind)
			err = s.RequireTaskWorkspaceArchiveEOF(ctx)
			if ending == "tls-close-notify" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if ending == "raw-close" {
					require.ErrorIs(t, err, io.ErrUnexpectedEOF, "raw EOF at TLS record boundary is truncation")
				}
				require.Error(t, s.RequireTaskWorkspaceArchiveEOF(ctx), "retry cannot discard trailing bytes and manufacture EOF proof")
			}
			require.NoError(t, <-done)
		})
	}
}

func TestTaskWorkspaceArchiveSessionRejectsInvalidAndTruncatedFrames(t *testing.T) {
	for _, fault := range []string{"bad-kind", "zero", "oversize", "partial-header", "partial-payload", "EOF-before-complete"} {
		t.Run(fault, func(t *testing.T) {
			s, done := archiveSessionPair(t, func(conn *tls.Conn, _ net.Conn, _ RuntimeRequest) error {
				var header [5]byte
				header[0] = ArchiveData
				binary.BigEndian.PutUint32(header[1:], 2)
				wire := header[:]
				switch fault {
				case "bad-kind":
					header[0] = EventAccepted
				case "zero":
					binary.BigEndian.PutUint32(header[1:], 0)
				case "oversize":
					binary.BigEndian.PutUint32(header[1:], 32769)
				case "partial-header":
					wire = header[:2]
				case "partial-payload":
					wire = append(wire, 1)
				case "EOF-before-complete":
					wire = nil
				}
				if len(wire) > 0 {
					if _, err := conn.Write(wire); err != nil {
						return err
					}
				}
				return conn.CloseWrite()
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			require.NoError(t, s.SendTaskWorkspaceArchive(ctx, archiveEnvelope()))
			_, _, err := s.ReadTaskWorkspaceArchiveFrame(ctx)
			require.Error(t, err)
			require.Error(t, s.RequireTaskWorkspaceArchiveEOF(ctx))
			require.NoError(t, <-done)
		})
	}
}

func TestTaskWorkspaceArchiveSessionRejectsEventModesAndFailedSendReplay(t *testing.T) {
	for _, mode := range []string{"exec", "close", "quiesce"} {
		t.Run(mode, func(t *testing.T) {
			s, done := archiveSessionPair(t, func(conn *tls.Conn, _ net.Conn, r RuntimeRequest) error {
				if r.Archive != nil {
					return ErrFrame
				}
				return WriteEvent(conn, EventAccepted, []byte("owned"))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var err error
			switch mode {
			case "exec":
				err = s.Send(ctx, frameRequest(t))
			case "close":
				err = s.SendTaskClose(ctx, taskFrame())
			case "quiesce":
				err = s.SendTaskQuiesce(ctx, quiesceFrame())
			}
			require.NoError(t, err)
			_, _, err = s.ReadTaskWorkspaceArchiveFrame(ctx)
			require.ErrorIs(t, err, ErrFrame)
			require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(ctx), ErrFrame)
			kind, wire, err := s.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, EventAccepted, kind)
			require.Equal(t, "owned", string(wire))
			require.ErrorIs(t, s.SendTaskWorkspaceArchive(ctx, archiveEnvelope()), ErrFrame)
			require.NoError(t, <-done)
		})
	}
	s, _ := archiveSessionPair(t, func(*tls.Conn, net.Conn, RuntimeRequest) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, s.SendTaskWorkspaceArchive(ctx, archiveEnvelope()), context.Canceled)
	require.ErrorIs(t, s.SendTaskWorkspaceArchive(context.Background(), archiveEnvelope()), ErrFrame)
	for _, sealed := range []*Session{nil, {}, s} {
		if sealed == s {
			sealed = &Session{self: s, conn: s.conn, archiveTransport: s.archiveTransport}
		}
		require.ErrorIs(t, sealed.SendTaskWorkspaceArchive(context.Background(), archiveEnvelope()), ErrDestination)
		_, _, err := sealed.ReadTaskWorkspaceArchiveFrame(context.Background())
		require.ErrorIs(t, err, ErrDestination)
		require.ErrorIs(t, sealed.RequireTaskWorkspaceArchiveEOF(context.Background()), ErrDestination)
	}
}

func TestTaskWorkspaceArchiveSessionResponseContexts(t *testing.T) {
	for _, phase := range []string{"frame", "EOF"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			s, _ := archiveSessionPair(t, func(conn *tls.Conn, _ net.Conn, _ RuntimeRequest) error {
				if phase == "EOF" {
					if err := WriteTaskWorkspaceArchiveFrame(conn, ArchiveComplete, []byte("receipt")); err != nil {
						return err
					}
				}
				select {
				case <-release:
				case <-time.After(2 * time.Second):
					return errors.New("context test release timed out")
				}
				return nil
			})
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			require.NoError(t, s.SendTaskWorkspaceArchive(ctx, archiveEnvelope()))
			if phase == "EOF" {
				_, _, err := s.ReadTaskWorkspaceArchiveFrame(ctx)
				require.NoError(t, err)
			}
			canceled, cancelRead := context.WithCancel(context.Background())
			cancelRead()
			if phase == "frame" {
				_, _, err := s.ReadTaskWorkspaceArchiveFrame(nil)
				require.ErrorIs(t, err, ErrDestination)
				_, _, err = s.ReadTaskWorkspaceArchiveFrame(canceled)
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(nil), ErrDestination)
				require.ErrorIs(t, s.RequireTaskWorkspaceArchiveEOF(canceled), context.Canceled)
			}
			bounded, cancelBounded := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancelBounded()
			start := time.Now()
			if phase == "frame" {
				_, _, err := s.ReadTaskWorkspaceArchiveFrame(bounded)
				require.Error(t, err)
			} else {
				require.Error(t, s.RequireTaskWorkspaceArchiveEOF(bounded))
			}
			require.Less(t, time.Since(start), time.Second, "response context bounds blocked physical TLS IO")
		})
	}
}

func archiveTransportContext(t *testing.T, o DestinationOptions, issuer []byte, now time.Time) p.TaskWorkspaceArchiveContext {
	t.Helper()
	q := quiesceTransportContext(t, o, issuer, now)
	q.Current.CommandID = "ba1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	c := q.Current
	c.CommandID = "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	return p.TaskWorkspaceArchiveContext{Current: c, QuiescenceContext: q, QuiescenceTicketDigest: strings.Repeat("e", 64), QuiescenceReceiptDigest: strings.Repeat("f", 64), PlanDigest: strings.Repeat("a", 64), QuiescenceIntentRevision: 14, FinalSyncRevision: 15}
}

func TestTaskWorkspaceArchiveDestinationRetainedTrustAndSessionCoverage(t *testing.T) {
	o, _, issuer, clock, key, _ := destinationFixture(t)
	o.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(o)
	require.NoError(t, err)
	c := archiveTransportContext(t, o, issuer, clock.now)
	require.NoError(t, c.Validate())
	require.True(t, d.MatchTaskWorkspaceArchive(c, issuer))
	before, after, session := clock.now.Add(-2*time.Second), clock.now.Add(20*time.Second), clock.now.Add(40*time.Second)
	digest := strings.Repeat("b", 64)
	wire, err := p.SignTaskWorkspaceArchiveReceipt(key, p.TaskWorkspaceArchiveReceiptClaims{Version: 1, State: "archive_complete", Context: c, TicketDigest: digest, NotBefore: before, NotAfter: after, SessionNotAfter: session, ArchiveBytes: 1536, EntryCount: 1, SHA256: strings.Repeat("c", 64)})
	require.NoError(t, err)
	require.NoError(t, d.VerifyTaskWorkspaceArchiveRuntime(o.Verifier, c, before, after, session, clock.now))
	proof, err := d.VerifyTaskWorkspaceArchiveReceipt(o.Verifier, wire, c, digest, before, after, session, session.Add(time.Minute))
	require.NoError(t, err, "historical receipt does not replay a producer")
	require.Equal(t, uint64(1536), proof.ArchiveBytes())
	other, _, _, _, _, _ := destinationFixture(t)
	for _, bad := range []struct {
		verifier                    *p.ManagementVerifier
		context                     p.TaskWorkspaceArchiveContext
		digest                      string
		before, after, session, now time.Time
	}{
		{other.Verifier, c, digest, before, after, session, clock.now},
		{o.Verifier, c, "wrong", before, after, session, clock.now},
		{o.Verifier, c, digest, before.Add(-time.Second), after, session, clock.now},
		{o.Verifier, c, digest, before, after.Add(time.Second), session, clock.now},
		{o.Verifier, c, digest, before, after, session.Add(time.Second), clock.now},
		{o.Verifier, c, digest, before, after, clock.now.Add(2 * time.Hour), clock.now},
		{o.Verifier, c, digest, before, after, session, clock.now.Add(2 * time.Hour)},
	} {
		proof, err := d.VerifyTaskWorkspaceArchiveReceipt(bad.verifier, wire, bad.context, bad.digest, bad.before, bad.after, bad.session, bad.now)
		require.Error(t, err)
		require.Equal(t, p.TaskWorkspaceArchiveReceiptEvidence{}, proof)
	}
	for _, destination := range []*Destination{nil, {}, d} {
		if destination == d {
			copy := *d
			destination = &copy
		}
		require.False(t, destination.MatchTaskWorkspaceArchive(c, issuer))
		require.Error(t, destination.VerifyTaskWorkspaceArchiveRuntime(o.Verifier, c, before, after, session, clock.now))
	}
	c.Current.Runtime.BootID = "foreign"
	require.False(t, d.MatchTaskWorkspaceArchive(c, issuer))
	require.Error(t, d.VerifyTaskWorkspaceArchiveRuntime(o.Verifier, c, before, after, session, clock.now))
}

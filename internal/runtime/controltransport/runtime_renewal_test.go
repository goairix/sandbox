package controltransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func renewalFrameFixture() RuntimeRenewalEnvelope {
	return RuntimeRenewalEnvelope{
		Version: 1, Purpose: RuntimeRenewalQueryPurpose,
		Identity:      p.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: "6a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"}},
		DataGateEpoch: 2, Nonce: strings.Repeat("b", 64), PreviousDigest: strings.Repeat("c", 64), CandidateDigest: strings.Repeat("d", 64),
	}
}

// These are structural framing models, not signed activation or PID1 proof.
func TestRuntimeRenewalFramesStrictAndFinite(t *testing.T) {
	e := renewalFrameFixture()
	var frame bytes.Buffer
	require.NoError(t, WriteRuntimeRenewalRequest(&frame, e))
	wire := bytes.Clone(frame.Bytes())
	request, err := ReadRuntimeRequest(bytes.NewReader(wire))
	require.NoError(t, err)
	require.NotNil(t, request.Renewal)
	require.Equal(t, e, *request.Renewal)

	header := bytes.Clone(wire[4:])
	for name, bad := range map[string][]byte{
		"duplicate":     bytes.Replace(header, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":       append([]byte(`{"unknown":0,`), header[1:]...),
		"noncanonical":  append([]byte(" "), header...),
		"query-payload": bytes.Replace(header, []byte(`"purpose":"runtime_activation_renew_query"`), []byte(`"purpose":"runtime_activation_renew_query","activation":"eA=="`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			var size [4]byte
			binary.BigEndian.PutUint32(size[:], uint32(len(bad)))
			_, err := ReadRuntimeRequest(bytes.NewReader(append(size[:], bad...)))
			require.Error(t, err)
		})
	}
	for _, bad := range [][]byte{wire[:len(wire)-1], append(bytes.Clone(wire), 0)} {
		_, err := ReadRuntimeRequest(bytes.NewReader(bad))
		require.Error(t, err, "truncated or trailing request must fail before dispatch")
	}
	e.Purpose = RuntimeRenewalPurpose
	e.Activation, e.RuntimeCertificate, e.IssuerCertificate = []byte("activation"), []byte("runtime"), []byte("issuer")
	frame.Reset()
	require.NoError(t, WriteRuntimeRenewalRequest(&frame, e))
	request, err = ReadRuntimeRequest(&frame)
	require.NoError(t, err)
	require.Equal(t, e, *request.Renewal)
	e.Activation = bytes.Repeat([]byte{'x'}, 16385)
	require.Error(t, WriteRuntimeRenewalRequest(&bytes.Buffer{}, e))
}

type renewalCloseErrorConn struct {
	net.Conn
	err error
}

func (c *renewalCloseErrorConn) Close() error {
	return errors.Join(c.Conn.Close(), c.err)
}

// A real mTLS exchange exercises response attribution, authenticated EOF,
// adapter terminal EOF and final Session.Close. The peer's response is only a
// wire diagnostic model; supervisor/journal tests cover actual installation.
func TestRuntimeRenewalDestinationTLS(t *testing.T) {
	for _, mode := range []string{"success", "closed", "rejected-query", "rejected-install", "nonce", "candidate", "gate", "trailing", "adapter-trailing", "no-close-notify", "close-error"} {
		t.Run(mode, func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			closeFailure := errors.New("original adapter close failed")
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil || mode != "close-error" {
					return conn, err
				}
				return &renewalCloseErrorConn{Conn: conn, err: closeFailure}, nil
			}
			d, err := NewDestination(o)
			require.NoError(t, err)
			done := make(chan error, 1)
			finished := make(chan struct{})
			var once sync.Once
			var serverErr error
			join := func() {
				once.Do(func() {
					cancel()
					listener.Close()
					serverErr = <-done
					<-finished
				})
			}
			t.Cleanup(join)
			go func() {
				defer close(finished)
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				conn := tls.Server(raw, server)
				request, err := ReadRuntimeRequest(conn)
				if err != nil {
					done <- err
					return
				}
				e := request.Renewal
				purpose := RuntimeRenewalQueryPurpose
				if mode == "rejected-install" {
					purpose = RuntimeRenewalPurpose
				}
				if e == nil || e.Purpose != purpose || e.Identity != o.Identity || e.DataGateEpoch != 2 {
					done <- errors.New("wrong original renewal attribution")
					return
				}
				response := RuntimeRenewalResponse{Version: 1, Purpose: "runtime_activation_renewed", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Installed: true, GateState: "open"}
				switch mode {
				case "closed":
					response.GateState = "closed"
				case "rejected-query", "rejected-install":
					response.Installed = false
					response.Rejected = true
				case "nonce":
					response.Nonce = strings.Repeat("f", 64)
				case "candidate":
					response.CandidateDigest = strings.Repeat("f", 64)
				case "gate":
					response.DataGateEpoch++
				}
				err = WriteRuntimeRenewalResponse(conn, response)
				if err == nil && mode == "trailing" {
					_, err = conn.Write([]byte{0})
				}
				if err == nil && mode != "no-close-notify" {
					err = conn.CloseWrite()
				}
				if err == nil && mode == "adapter-trailing" {
					// tls.CloseWrite expires the underlying write deadline after
					// close_notify. Restore only this fixture's original bound so
					// the malformed adapter tail is actually sent, not timed out.
					deadline, _ := ctx.Deadline()
					err = raw.SetWriteDeadline(deadline)
					if err == nil {
						_, err = raw.Write([]byte{255})
					}
				}
				done <- err
			}()
			var response RuntimeRenewalResponse
			if mode == "rejected-install" {
				// Structural model of a completed refusal only, not a signed
				// candidate or a supervisor installation-positive assertion.
				e := renewalFrameFixture()
				e.Identity = o.Identity
				e.Purpose = RuntimeRenewalPurpose
				e.Activation, e.RuntimeCertificate, e.IssuerCertificate = []byte("model"), []byte("model"), []byte("model")
				response, err = d.exchangeRuntimeRenewal(ctx, e)
			} else {
				response, err = d.QueryRuntimeActivationRenewal(ctx, 2, strings.Repeat("c", 64), strings.Repeat("d", 64))
			}
			if mode == "success" || mode == "closed" || mode == "rejected-install" {
				require.NoError(t, err)
				require.Equal(t, mode != "rejected-install", response.Installed)
				require.Equal(t, mode == "rejected-install", response.Rejected)
				require.Equal(t, o.Identity, response.Identity)
			} else {
				require.Error(t, err)
				require.Equal(t, RuntimeRenewalResponse{}, response)
				if mode == "close-error" {
					require.ErrorIs(t, err, closeFailure)
				}
			}
			join()
			if mode == "success" || mode == "closed" || mode == "close-error" || mode == "rejected-install" || mode == "rejected-query" || mode == "adapter-trailing" {
				require.NoError(t, serverErr, "original TLS handler must physically return")
			}
		})
	}
}

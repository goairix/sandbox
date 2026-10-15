package controltransport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// The one-byte terminal probe is observable without replacing the actual TCP
// transport. crypto/tls reads whole records into its own larger input buffer.
type publicationTerminalTCP struct {
	*net.TCPConn
	terminal chan struct{}
	once     sync.Once
}

func (c *publicationTerminalTCP) Read(b []byte) (int, error) {
	if len(b) == 1 {
		c.once.Do(func() { close(c.terminal) })
	}
	return c.TCPConn.Read(b)
}

func TestRuntimePublicationDestinationTerminal(t *testing.T) {
	for _, mode := range []string{"success", "trailing", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			terminal := make(chan struct{})
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil {
					return nil, err
				}
				return &publicationTerminalTCP{TCPConn: raw.(*net.TCPConn), terminal: terminal}, nil
			}
			d, err := NewDestination(o)
			if err != nil {
				t.Fatal(err)
			}
			gate := make(chan struct{})
			var release sync.Once
			unblock := func() { release.Do(func() { close(gate) }) }
			serverDone := make(chan error, 1)
			serverFinished := make(chan struct{})
			clientDone := make(chan error, 1)
			response := publicationFrameResponse()
			go func() {
				defer close(serverFinished)
				raw, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				conn := tls.Server(raw, server)
				_, err = ReadRuntimeRequest(conn)
				if err == nil {
					err = WriteRuntimePublicationResponse(conn, response)
				}
				if err == nil {
					err = conn.CloseWrite()
				}
				if err == nil {
					<-gate
					if mode == "trailing" {
						_ = raw.SetWriteDeadline(time.Now().Add(time.Second))
						_, err = raw.Write([]byte{255})
					}
				}
				serverDone <- err
			}()
			go func() {
				claim := p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}
				enrollment, ready, err := d.ReadEnrolledPublication(ctx, []byte("certificate"), claim)
				if err == nil && (!reflect.DeepEqual(enrollment, response.Enrollment) || !reflect.DeepEqual(ready, response.Ready)) {
					err = errors.New("publication response mismatch")
				}
				clientDone <- err
			}()
			var clientErr error
			var joined sync.Once
			join := func() { joined.Do(func() { clientErr = <-clientDone }) }
			t.Cleanup(func() {
				cancel()
				unblock()
				listener.Close()
				join()
				err := <-serverDone
				<-serverFinished
				if err != nil && !t.Failed() {
					t.Errorf("original TLS server: %v", err)
				}
			})
			select {
			case clientErr = <-clientDone:
				joined.Do(func() {})
				t.Fatalf("publication completed before physical terminal gate: %v", clientErr)
			case <-terminal:
			case <-ctx.Done():
				t.Fatal("publication never reached physical terminal wait")
			}
			switch mode {
			case "cancel":
				cancel()
			case "deadline":
				// Keep the original server gate shut until the caller's deadline
				// itself terminates the entered physical read.
			case "success", "trailing":
				unblock()
			}
			join()
			if mode == "success" && clientErr != nil {
				t.Fatal(clientErr)
			}
			if mode != "success" && (clientErr == nil || errors.Is(clientErr, io.EOF)) {
				t.Fatalf("terminal failure accepted: %v", clientErr)
			}
		})
	}
}

func TestRuntimePublicationDestinationTLS(t *testing.T) {
	for _, mode := range []string{"positive", "trailing", "no-close-notify", "oversize", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var dials, requests atomic.Int32
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				dials.Add(1)
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}
			d, err := NewDestination(o)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			certificate := []byte("opaque-publication-certificate")
			claim := p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}
			response := publicationFrameResponse()
			done := make(chan error, 1)
			finished := make(chan struct{})
			var joinOnce sync.Once
			var serverErr error
			join := func() {
				joinOnce.Do(func() {
					cancel()
					_ = listener.Close()
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
				_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
				conn := tls.Server(raw, server)
				request, err := ReadRuntimeRequest(conn)
				if err != nil {
					done <- err
					return
				}
				requests.Add(1)
				if request.Publication == nil || string(request.Publication.PublicationCertificate) != string(certificate) || request.Publication.Claim != claim {
					done <- fmt.Errorf("wrong request attribution")
					return
				}
				if mode == "cancel" {
					cancel()
					done <- nil
					return
				}
				if mode == "oversize" {
					var size [4]byte
					binary.BigEndian.PutUint32(size[:], MaxRuntimePublicationResponse+1)
					_, err = conn.Write(size[:])
				} else {
					err = WriteRuntimePublicationResponse(conn, response)
				}
				if err == nil && mode == "trailing" {
					_, err = conn.Write([]byte{0})
				}
				if err == nil && mode != "no-close-notify" {
					err = conn.CloseWrite()
				}
				done <- err
			}()
			enrollment, ready, err := d.ReadEnrolledPublication(ctx, certificate, claim)
			if mode == "positive" {
				if err != nil || !reflect.DeepEqual(enrollment, response.Enrollment) || !reflect.DeepEqual(ready, response.Ready) {
					t.Fatalf("copied response mismatch: %v", err)
				}
				ready[0] = 'x'
				enrollment.Request.Activation[0] = 'x'
				if response.Ready[0] == 'x' || response.Enrollment.Request.Activation[0] == 'x' {
					t.Fatal("response aliases server memory")
				}
			} else if err == nil || len(ready) != 0 || len(enrollment.Receipt) != 0 {
				t.Fatal("invalid response returned successful evidence")
			}
			// This is the original TLS worker, joined even for canceled/invalid IO.
			join()
			if mode == "positive" && serverErr != nil {
				t.Fatal(serverErr)
			}
			if dials.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("implicit retry: dials=%d requests=%d", dials.Load(), requests.Load())
			}
		})
	}
}

func TestRuntimePublicationDestinationSingleSend(t *testing.T) {
	o, server, _, _, _, _ := destinationFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	o.Dial = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	var joinOnce sync.Once
	var serverErr error
	join := func() {
		joinOnce.Do(func() {
			cancel()
			_ = listener.Close()
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
		_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
		conn := tls.Server(raw, server)
		_, err = ReadRuntimeRequest(conn)
		if err == nil {
			err = WriteRuntimePublicationResponse(conn, publicationFrameResponse())
		}
		if err == nil {
			err = conn.CloseWrite()
		}
		done <- err
	}()
	s, err := d.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := RuntimePublicationEnvelope{Version: 1, Purpose: RuntimeEnrolledPublicationPurpose, PublicationCertificate: []byte("certificate"), Claim: p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}}
	if err := s.sendEnrolledPublication(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.sendEnrolledPublication(ctx, e); err == nil {
		t.Fatal("second publication request accepted")
	}
	if _, _, err := s.Read(ctx); err == nil {
		t.Fatal("publication response treated as legacy events")
	}
	if _, err := s.readEnrolledPublication(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.readEnrolledPublication(ctx); err == nil {
		t.Fatal("second publication response read accepted")
	}
	join()
	if serverErr != nil {
		t.Fatal(serverErr)
	}
}

func TestRuntimePublicationDestinationRejectsUntrusted(t *testing.T) {
	o, _, _, _, _, _ := destinationFixture(t)
	var dials atomic.Int32
	o.Dial = func(context.Context) (net.Conn, error) { dials.Add(1); return nil, errors.New("unexpected dial") }
	d, err := NewDestination(o)
	if err != nil {
		t.Fatal(err)
	}
	copy := *d
	for _, target := range []*Destination{nil, {}, &copy} {
		if _, _, err := target.ReadEnrolledPublication(context.Background(), nil, p.ClaimReference{}); err == nil {
			t.Fatal("untrusted destination accepted")
		}
	}
	if _, _, err := d.ReadEnrolledPublication(context.Background(), make([]byte, 4097), p.ClaimReference{}); err == nil || dials.Load() != 0 {
		t.Fatal("oversized input reached transport")
	}
}

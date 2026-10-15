package controltransport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

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

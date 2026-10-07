package controlrunner

import (
	"context"
	"net"
	"testing"
)

func TestSupervisorTransportRejectsUntrustedOwner(t *testing.T) {
	for _, s := range []*Supervisor{nil, {}} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Serve(context.Background(), listener); err == nil {
			t.Fatal("untrusted owner served")
		}
		if conn, err := net.Dial("tcp", listener.Addr().String()); err == nil {
			conn.Close()
			t.Fatal("rejected listener not closed")
		}
	}
}

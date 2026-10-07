//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"os"
	"testing"
	"time"
)

func TestTask3NativeRenewalExpiry(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, controlprotocol.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 10}, 4*time.Second)
	waitExecutionReady(t, e)
	e.mu.Lock()
	originalRecord := e.record
	e.mu.Unlock()
	now := time.Now().UTC()
	wire, err := controlprotocol.SignExecRenewTicket(f.issuer, controlprotocol.ExecRenewTicketClaims{Version: 1, Purpose: "operation_exec_renew", Context: originalRecord.Context, DescriptorDigest: originalRecord.DescriptorDigest, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(6 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := f.s.options.Verifier.VerifyExecRenewTicket(wire, f.issuerWire, originalRecord.Context, originalRecord.DescriptorDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = f.s.renew(context.Background(), e, evidence); err != nil {
		t.Fatal(err)
	}
	drainExecution(t, e)
	e.mu.Lock()
	record := e.record
	e.mu.Unlock()
	if time.Since(start) < 4*time.Second || record.Reason != "authority_expired" || record.RootWaitStatus != 9 || !record.DrainConfirmed {
		t.Fatalf("renewal expiry %+v elapsed=%v", record, time.Since(start))
	}
	if err = f.s.renew(context.Background(), e, evidence); err == nil {
		t.Fatal("terminal execution resurrected")
	}
	t.Logf("durable renewal ACK then actual expiry raw=%d reason=%s elapsed=%v", record.RootWaitStatus, record.Reason, time.Since(start))
}

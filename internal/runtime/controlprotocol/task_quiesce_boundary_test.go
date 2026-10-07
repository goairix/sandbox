package controlprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTaskQuiesceReceiptExecutionDigest64Mixed(t *testing.T) {
	f := execStartSetup(t)
	entries := make([]TaskQuiescenceExecution, 64)
	parts := make([]string, 64)
	for i := range entries {
		c := f.claims.Context
		c.CommandID = fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		disposition, digest := "never_spawned", ""
		if i%2 == 1 {
			disposition = "local_terminal"
			digest = strings.Repeat("b", 64)
		}
		entries[i] = TaskQuiescenceExecution{CommandID: c.CommandID, ExecContext: c, TicketDigest: strings.Repeat("a", 64), Disposition: disposition, TerminalReceiptDigest: digest}
		contextWire, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		parts[i] = fmt.Sprintf(`{"command_id":%q,"exec_context":%s,"ticket_digest":%q,"disposition":%q,"terminal_receipt_digest":%q}`, c.CommandID, contextWire, entries[i].TicketDigest, disposition, digest)
	}
	expected := sha256.Sum256([]byte("[" + strings.Join(parts, ",") + "]"))
	got, err := DigestTaskQuiescenceExecutions(entries)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(expected[:]) {
		t.Fatalf("64 mixed canonical digest %s", got)
	}
}

package controlprotocol

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestTaskQuiesceReceiptExecutionDigest(t *testing.T) {
	f := execStartSetup(t)
	empty, err := DigestTaskQuiescenceExecutions(nil)
	require.NoError(t, err)
	require.Equal(t, wireDigest([]byte("[]")), empty)
	e := TaskQuiescenceExecution{CommandID: f.claims.Context.CommandID, ExecContext: f.claims.Context, TicketDigest: strings.Repeat("a", 64), Disposition: "never_spawned"}
	canonical := `[{"command_id":"` + e.CommandID + `","exec_context":`
	contextWire, err := json.Marshal(e.ExecContext)
	require.NoError(t, err)
	canonical += string(contextWire) + `,"ticket_digest":"` + e.TicketDigest + `","disposition":"never_spawned","terminal_receipt_digest":""}]`
	d, err := DigestTaskQuiescenceExecutions([]TaskQuiescenceExecution{e})
	require.NoError(t, err)
	require.Equal(t, wireDigest([]byte(canonical)), d)
	terminal := e
	terminal.CommandID = "ba1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	terminal.ExecContext.CommandID = terminal.CommandID
	terminal.Disposition = "local_terminal"
	terminal.TerminalReceiptDigest = strings.Repeat("b", 64)
	_, err = DigestTaskQuiescenceExecutions([]TaskQuiescenceExecution{e, terminal})
	require.NoError(t, err)
	for _, bad := range [][]TaskQuiescenceExecution{{e, e}, {terminal, e}, make([]TaskQuiescenceExecution, 65)} {
		d, err = DigestTaskQuiescenceExecutions(bad)
		require.Error(t, err)
		require.Empty(t, d)
	}
	for name, mutate := range map[string]func(*TaskQuiescenceExecution){"command": func(x *TaskQuiescenceExecution) { x.CommandID = terminal.CommandID }, "context": func(x *TaskQuiescenceExecution) { x.ExecContext.LeaseID = 0 }, "digest": func(x *TaskQuiescenceExecution) { x.TicketDigest = "" }, "disposition": func(x *TaskQuiescenceExecution) { x.Disposition = "unknown" }, "never-receipt": func(x *TaskQuiescenceExecution) { x.TerminalReceiptDigest = terminal.TerminalReceiptDigest }, "terminal-no-receipt": func(x *TaskQuiescenceExecution) { x.Disposition = "local_terminal" }} {
		t.Run(name, func(t *testing.T) {
			bad := e
			mutate(&bad)
			d, err := DigestTaskQuiescenceExecutions([]TaskQuiescenceExecution{bad})
			require.Error(t, err)
			require.Empty(t, d)
		})
	}
}

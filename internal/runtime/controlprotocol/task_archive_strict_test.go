package controlprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskWorkspaceArchiveStrictJSON(t *testing.T) {
	f, c := archiveSetup(t)
	r := archiveReceipt(c)
	for _, receipt := range []bool{false, true} {
		name, limit := "ticket", 8192
		w := taskCloseRaw(t, f.issuerKey, c, "sandbox-task-workspace-archive-ticket:v1\x00")
		if receipt {
			name, limit = "receipt", 16384
			w = taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
		}
		t.Run(name, func(t *testing.T) {
			verify := func(b []byte) []byte {
				if receipt {
					e, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(b, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
					require.NoError(t, err)
					return e.Wire()
				}
				e, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(b, f.issuerWire, c.Context, f.now)
				require.NoError(t, err)
				return e.Wire()
			}
			reject := func(t *testing.T, b []byte) {
				if receipt {
					e, err := f.verifier.VerifyTaskWorkspaceArchiveReceipt(b, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
					require.Error(t, err)
					require.Equal(t, TaskWorkspaceArchiveReceiptEvidence{}, e)
				} else {
					e, err := f.verifier.VerifyTaskWorkspaceArchiveTicket(b, f.issuerWire, c.Context, f.now)
					require.Error(t, err)
					require.Equal(t, TaskWorkspaceArchiveEvidence{}, e)
				}
			}
			taskStrictCases(t, w, reject)
			archiveWrongTypes(t, w, reject)
			padded := append(bytes.Clone(w), bytes.Repeat([]byte(" "), limit-len(w))...)
			require.Equal(t, w, verify(padded))
			reject(t, append(padded, ' '))
			pretty := new(bytes.Buffer)
			require.NoError(t, json.Indent(pretty, w, "", "  "))
			require.Equal(t, w, verify(pretty.Bytes()))
		})
	}
}

func archiveWrongTypes(t *testing.T, valid []byte, reject func(*testing.T, []byte)) {
	t.Helper()
	var walk func([]byte, []string)
	walk = func(object []byte, path []string) {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(object, &fields))
		for name, value := range fields {
			full := append(append([]string(nil), path...), name)
			t.Run(strings.Join(full, "/")+"/wrong-type", func(t *testing.T) {
				reject(t, runtimeIdentityMutate(t, valid, full, json.RawMessage("true"), false))
			})
			if len(value) > 0 && value[0] == '{' {
				walk(value, full)
			}
		}
	}
	walk(valid, nil)
}

func TestTaskWorkspaceArchiveIndependentInvalidContextsAndTimes(t *testing.T) {
	f, c := archiveSetup(t)
	for _, path := range []string{"current", "quiescence-current", "close-history"} {
		for name, mutate := range taskInvalidContexts() {
			t.Run(path+"/"+name, func(t *testing.T) {
				bad := c
				switch path {
				case "current":
					mutate(&bad.Context.Current)
				case "quiescence-current":
					mutate(&bad.Context.QuiescenceContext.Current)
				case "close-history":
					mutate(&bad.Context.QuiescenceContext.CloseDataContext)
				}
				_, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, bad)
				require.Error(t, err)
				w := taskCloseRaw(t, f.issuerKey, bad, "sandbox-task-workspace-archive-ticket:v1\x00")
				_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(w, f.issuerWire, c.Context, f.now)
				require.Error(t, err)
				r := archiveReceipt(bad)
				_, err = SignTaskWorkspaceArchiveReceipt(f.runtimeKey, r)
				require.Error(t, err)
				w = taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
				_, err = f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
				require.Error(t, err)
			})
		}
	}
	for name, times := range taskBadTimes(c.NotBefore, c.NotAfter) {
		t.Run(name, func(t *testing.T) {
			bad := c
			bad.NotBefore, bad.NotAfter = times[0], times[1]
			_, err := SignTaskWorkspaceArchiveTicket(f.issuerKey, f.issuer, bad)
			require.Error(t, err)
			w := taskCloseRaw(t, f.issuerKey, bad, "sandbox-task-workspace-archive-ticket:v1\x00")
			_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(w, f.issuerWire, c.Context, f.now)
			require.Error(t, err)
			r := archiveReceipt(bad)
			_, err = SignTaskWorkspaceArchiveReceipt(f.runtimeKey, r)
			require.Error(t, err)
			w = taskCloseRaw(t, f.runtimeKey, r, "sandbox-task-workspace-archive-complete:v1\x00")
			_, err = f.verifier.VerifyTaskWorkspaceArchiveReceipt(w, f.runtimeWire, c.Context, r.TicketDigest, c.NotBefore, c.NotAfter, c.SessionNotAfter, f.now)
			require.Error(t, err)
		})
	}
}

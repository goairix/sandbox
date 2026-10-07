//go:build linux || darwin

package controltarget

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskQuiesceHistorySchema(t *testing.T) {
	f := quiesceJournalSetup(t)
	for _, state := range []string{"pending", "users_quiesced"} {
		t.Run(state, func(t *testing.T) {
			r := TaskUserQuiescenceRecord{Version: 1, State: state, Context: f.e.Context(), TicketDigest: f.e.Digest(), NotBefore: f.e.NotBefore(), NotAfter: f.e.NotAfter()}
			if state == "users_quiesced" {
				r.ExecutionSetDigest = digestJournalBytes([]byte("[]"))
			}
			w, err := encodeTaskQuiescence(r)
			require.NoError(t, err)
			got, err := decodeTaskQuiescence(w)
			require.NoError(t, err)
			require.Equal(t, r, got)
			reject := func(w []byte) {
				got, err := decodeTaskQuiescence(w)
				require.Error(t, err)
				require.Equal(t, TaskUserQuiescenceRecord{}, got)
			}
			var root map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(w, &root))
			var walk func([]byte, []string)
			replace := func(path []string, value []byte) []byte {
				var tree map[string]any
				require.NoError(t, json.Unmarshal(w, &tree))
				parent := tree
				for _, part := range path[:len(path)-1] {
					parent = parent[part].(map[string]any)
				}
				parent[path[len(path)-1]] = json.RawMessage(value)
				out, err := json.Marshal(tree)
				require.NoError(t, err)
				return out
			}
			walk = func(obj []byte, path []string) {
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(obj, &fields))
				for key, value := range fields {
					full := append(append([]string{}, path...), key)
					t.Run(strings.Join(full, "/"), func(t *testing.T) {
						reject(replace(full, []byte("null")))
						var minus map[string]json.RawMessage
						require.NoError(t, json.Unmarshal(obj, &minus))
						delete(minus, key)
						missing, err := json.Marshal(minus)
						require.NoError(t, err)
						if len(path) == 0 {
							reject(missing)
						} else {
							reject(replace(path, missing))
						}
						k, err := json.Marshal(key)
						require.NoError(t, err)
						dupe := append(append(append(append([]byte("{"), k...), ':'), value...), ',')
						dupe = append(dupe, obj[1:]...)
						if len(path) == 0 {
							reject(dupe)
						} else {
							reject(replace(path, dupe))
						}
					})
					if len(value) > 0 && value[0] == '{' {
						walk(value, full)
					}
				}
			}
			walk(w, nil)
			reject(append(bytes.Clone(w), 'x'))
			reject(bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"unknown":0`), 1))
			reject(bytes.Replace(w, []byte(`"claim_create_revision":12`), []byte(`"claim_create_revision":12.0`), 1))
			if state == "pending" {
				reject(bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"execution_set_digest":""`), 1))
			} else {
				r.RegisteredCount = 65
				_, err = encodeTaskQuiescence(r)
				require.Error(t, err)
			}
		})
	}
}
func TestTaskQuiesceHistoryConflicts(t *testing.T) {
	for _, kind := range []string{"missing-close", "wrong-task", "duplicate-attempt", "symlink", "bad-temp-name", "open-gate"} {
		t.Run(kind, func(t *testing.T) {
			f := quiesceJournalSetup(t)
			_, err := f.j.AcceptUserQuiescence(context.Background(), f.e, f.receipt)
			require.NoError(t, err)
			r, err := f.j.LookupUserQuiescence(context.Background(), f.e.Context(), f.e.Digest())
			require.NoError(t, err)
			require.NoError(t, f.j.Close())
			path := filepath.Join(f.o.Directory, "users-quiesce.json")
			switch kind {
			case "missing-close":
				require.NoError(t, os.Remove(filepath.Join(f.o.Directory, "data-close.json")))
			case "wrong-task":
				r.Context.CloseDataContext.TaskID = r.Context.Current.CommandID
				r.Context.Current.TaskID = r.Context.Current.CommandID
				w, err := encodeTaskQuiescence(*r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, w, 0600))
			case "duplicate-attempt":
				r.Context.Current.CommandID = "d1111111-1111-4111-8111-111111111111"
				w, err := encodeTaskQuiescence(*r)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, ".users-quiesce.e1111111-1111-4111-8111-111111111111.tmp"), w, 0600))
			case "symlink":
				require.NoError(t, os.Rename(path, path+".other"))
				require.NoError(t, os.Symlink(path+".other", path))
			case "bad-temp-name":
				require.NoError(t, os.Rename(path, filepath.Join(f.o.Directory, ".users-quiesce.bad.tmp")))
			case "open-gate":
				w, err := os.ReadFile(filepath.Join(f.o.Directory, "gate.json"))
				require.NoError(t, err)
				w = bytes.Replace(w, []byte(`"gate_state":"closed"`), []byte(`"gate_state":"open"`), 1)
				require.NoError(t, os.WriteFile(filepath.Join(f.o.Directory, "gate.json"), w, 0600))
			}
			cold, err := OpenClosedJournal(context.Background(), f.o)
			require.Error(t, err)
			require.Nil(t, cold)
		})
	}
}

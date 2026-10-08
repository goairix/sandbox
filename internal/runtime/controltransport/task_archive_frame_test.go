package controltransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func archiveEnvelope() TaskWorkspaceArchiveEnvelope {
	// Context semantics are deliberately left to protocol authentication.
	return TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive", TicketDigest: strings.Repeat("a", 64), Ticket: []byte{0, 255}, IssuerCertificate: []byte("issuer"), OriginalQuiescenceReceipt: []byte("receipt")}
}

func archiveSized(header []byte) []byte {
	b := make([]byte, 4+len(header))
	binary.BigEndian.PutUint32(b, uint32(len(header)))
	copy(b[4:], header)
	return b
}

func TestTaskWorkspaceArchiveCanonicalQuery(t *testing.T) {
	e := archiveEnvelope()
	e.Purpose = "task_workspace_archive_query"
	e.Ticket, e.IssuerCertificate, e.OriginalQuiescenceReceipt = nil, nil, nil
	var wire bytes.Buffer
	require.NoError(t, WriteTaskWorkspaceArchiveRequest(&wire, e), "attribution-only archive queries must be accepted")
	got, err := ReadTaskWorkspaceArchiveRequest(&wire)
	require.NoError(t, err)
	require.Equal(t, e, got)
	for _, purpose := range []string{"task_workspace_archive", "task_workspace_archive_query"} {
		e := archiveEnvelope()
		e.Purpose = purpose
		if purpose == "task_workspace_archive_query" {
			e.Ticket, e.IssuerCertificate, e.OriginalQuiescenceReceipt = nil, nil, nil
		}
		var wire bytes.Buffer
		require.NoError(t, WriteTaskWorkspaceArchiveRequest(&wire, e))
		request, err := ReadRuntimeRequest(&wire)
		require.NoError(t, err)
		require.Equal(t, &e, request.Archive)
		require.Nil(t, request.Quiesce)
		require.Nil(t, request.CloseData)
		require.Equal(t, Envelope{}, request.Exec)
		for _, mutate := range []func(*TaskWorkspaceArchiveEnvelope){
			func(e *TaskWorkspaceArchiveEnvelope) { e.Ticket = []byte{} },
			func(e *TaskWorkspaceArchiveEnvelope) { e.IssuerCertificate = []byte{} },
			func(e *TaskWorkspaceArchiveEnvelope) { e.OriginalQuiescenceReceipt = []byte{} },
		} {
			bad := e
			mutate(&bad)
			var wire bytes.Buffer
			require.Error(t, WriteTaskWorkspaceArchiveRequest(&wire, bad))
			require.Empty(t, wire.Bytes())
			header, err := json.Marshal(bad)
			require.NoError(t, err)
			_, err = ReadRuntimeRequest(bytes.NewReader(archiveSized(header)))
			require.Error(t, err)
		}
	}
}

func TestTaskWorkspaceArchiveRequestRoundTripAndBounds(t *testing.T) {
	for _, maximum := range []bool{false, true} {
		e := archiveEnvelope()
		if maximum {
			e.Ticket = bytes.Repeat([]byte{255}, 8192)
			e.IssuerCertificate = bytes.Repeat([]byte{0}, 4096)
			e.OriginalQuiescenceReceipt = bytes.Repeat([]byte{1}, 12288)
		}
		var b bytes.Buffer
		require.NoError(t, WriteTaskWorkspaceArchiveRequest(&b, e))
		header, err := json.Marshal(e)
		require.NoError(t, err)
		require.Equal(t, archiveSized(header), b.Bytes())
		got, err := ReadTaskWorkspaceArchiveRequest(&b)
		require.NoError(t, err)
		require.Equal(t, e, got)
	}
	// The outer header limit is inclusive even though semantic context
	// validation remains the verifier's responsibility.
	e := archiveEnvelope()
	h, err := json.Marshal(e)
	require.NoError(t, err)
	e.Context.Current.WorkerID = strings.Repeat("x", MaxHeader-len(h))
	var maximumHeader bytes.Buffer
	require.NoError(t, WriteTaskWorkspaceArchiveRequest(&maximumHeader, e))
	require.Equal(t, uint32(MaxHeader), binary.BigEndian.Uint32(maximumHeader.Bytes()[:4]))
	got, err := ReadTaskWorkspaceArchiveRequest(&maximumHeader)
	require.NoError(t, err)
	require.Equal(t, e, got)
	e.Context.Current.WorkerID += "x"
	require.Error(t, WriteTaskWorkspaceArchiveRequest(&maximumHeader, e))
	require.Empty(t, maximumHeader.Bytes())
	for name, mutate := range map[string]func(*TaskWorkspaceArchiveEnvelope){
		"version":       func(e *TaskWorkspaceArchiveEnvelope) { e.Version = 2 },
		"purpose":       func(e *TaskWorkspaceArchiveEnvelope) { e.Purpose = "task_workspace_archive_query" },
		"digest":        func(e *TaskWorkspaceArchiveEnvelope) { e.TicketDigest = strings.Repeat("A", 64) },
		"digest-short":  func(e *TaskWorkspaceArchiveEnvelope) { e.TicketDigest = "a" },
		"empty-ticket":  func(e *TaskWorkspaceArchiveEnvelope) { e.Ticket = nil },
		"empty-issuer":  func(e *TaskWorkspaceArchiveEnvelope) { e.IssuerCertificate = []byte{} },
		"empty-receipt": func(e *TaskWorkspaceArchiveEnvelope) { e.OriginalQuiescenceReceipt = nil },
		"large-ticket":  func(e *TaskWorkspaceArchiveEnvelope) { e.Ticket = make([]byte, 8193) },
		"large-issuer":  func(e *TaskWorkspaceArchiveEnvelope) { e.IssuerCertificate = make([]byte, 4097) },
		"large-receipt": func(e *TaskWorkspaceArchiveEnvelope) { e.OriginalQuiescenceReceipt = make([]byte, 12289) },
	} {
		t.Run(name, func(t *testing.T) {
			e := archiveEnvelope()
			mutate(&e)
			var b bytes.Buffer
			require.Error(t, WriteTaskWorkspaceArchiveRequest(&b, e))
			require.Empty(t, b.Bytes())
			header, err := json.Marshal(e)
			require.NoError(t, err)
			got, err := ReadTaskWorkspaceArchiveRequest(bytes.NewReader(archiveSized(header)))
			require.Error(t, err)
			require.Equal(t, TaskWorkspaceArchiveEnvelope{}, got)
		})
	}
}

func TestTaskWorkspaceArchiveRequestStrictAndEOF(t *testing.T) {
	header, err := json.Marshal(archiveEnvelope())
	require.NoError(t, err)
	var walk func([]byte, []string)
	mutate := func(path []string, replacement []byte, omit bool) []byte {
		var edit func([]byte, []string) []byte
		edit = func(raw []byte, p []string) []byte {
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &fields))
			key, err := json.Marshal(p[0])
			require.NoError(t, err)
			old := append(append(bytes.Clone(key), ':'), fields[p[0]]...)
			if len(p) == 1 && omit {
				if bytes.Contains(raw, append(bytes.Clone(old), ',')) {
					return bytes.Replace(raw, append(bytes.Clone(old), ','), nil, 1)
				}
				return bytes.Replace(raw, append([]byte{','}, old...), nil, 1)
			}
			value := replacement
			if len(p) > 1 {
				value = edit(fields[p[0]], p[1:])
			}
			return bytes.Replace(raw, old, append(append(bytes.Clone(key), ':'), value...), 1)
		}
		return edit(header, path)
	}
	reject := func(t *testing.T, h []byte) {
		_, err := ReadTaskWorkspaceArchiveRequest(bytes.NewReader(archiveSized(h)))
		require.Error(t, err)
		request, err := ReadRuntimeRequest(bytes.NewReader(archiveSized(h)))
		require.Error(t, err)
		require.Nil(t, request.Archive)
	}
	walk = func(raw []byte, path []string) {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		for name, value := range fields {
			full := append(append([]string(nil), path...), name)
			for _, kind := range []string{"missing", "null", "wrong-type"} {
				t.Run(strings.Join(full, "/")+"/"+kind, func(t *testing.T) {
					v := []byte("null")
					if kind == "wrong-type" {
						v = []byte("true")
					}
					reject(t, mutate(full, v, kind == "missing"))
				})
			}
			key, _ := json.Marshal(name)
			duplicate := append(append(append(append([]byte{'{'}, key...), ':'), value...), ',')
			duplicate = append(duplicate, raw[1:]...)
			t.Run(strings.Join(full, "/")+"/duplicate", func(t *testing.T) {
				if len(path) == 0 {
					reject(t, duplicate)
				} else {
					reject(t, mutate(path, duplicate, false))
				}
			})
			if len(value) > 0 && value[0] == '{' {
				walk(value, full)
			}
		}
		unknown := append([]byte(`{"unknown":true,`), raw[1:]...)
		t.Run(strings.Join(path, "/")+"/unknown", func(t *testing.T) {
			if len(path) == 0 {
				reject(t, unknown)
			} else {
				reject(t, mutate(path, unknown, false))
			}
		})
	}
	walk(header, nil)
	for _, h := range [][]byte{append([]byte(" "), header...), append(bytes.Clone(header), ' '), append(bytes.Clone(header), []byte("{}")...), bytes.Replace(header, []byte(`"version":1`), []byte(`"version":1.0`), 1)} {
		reject(t, h)
	}
	frame := archiveSized(header)
	_, err = ReadTaskWorkspaceArchiveRequest(bytes.NewReader(append(bytes.Clone(frame), 0)))
	require.Error(t, err)
	for i := 0; i < len(frame); i++ {
		_, err = ReadTaskWorkspaceArchiveRequest(bytes.NewReader(frame[:i]))
		require.Error(t, err)
	}
	for _, n := range []uint32{0, MaxHeader + 1, ^uint32(0)} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		r := &archiveHeaderOnlyReader{data: b[:]}
		_, err = ReadTaskWorkspaceArchiveRequest(r)
		require.Error(t, err)
		require.Equal(t, 1, r.reads, "invalid length must fail before payload read/allocation")
	}
	_, err = ReadTaskWorkspaceArchiveRequest(&archiveNoEOFReader{Reader: bytes.NewReader(frame)})
	require.Error(t, err)
	_, err = ReadTaskWorkspaceArchiveRequest(io.MultiReader(bytes.NewReader(frame), &archiveErrorReader{}))
	require.Error(t, err)
}

type archiveHeaderOnlyReader struct {
	data  []byte
	reads int
}

func (r *archiveHeaderOnlyReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, errors.New("unexpected payload read")
	}
	return copy(p, r.data), nil
}

type archiveNoEOFReader struct{ *bytes.Reader }

func (r *archiveNoEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		return 0, nil
	}
	return n, err
}

type archiveErrorReader struct{}

func (*archiveErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }

type archiveBadWriter struct {
	count  int
	err    error
	calls  int
	failAt int
}

func (w *archiveBadWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.failAt == 0 || w.calls == w.failAt {
		return w.count, w.err
	}
	return len(p), nil
}

func TestTaskWorkspaceArchiveFrameBinaryBoundsAndTruncation(t *testing.T) {
	for _, kind := range []byte{ArchiveData, ArchiveComplete} {
		maximum := 32768
		if kind == ArchiveComplete {
			maximum = 16384
		}
		for _, n := range []int{1, maximum} {
			data := make([]byte, n)
			for i := range data {
				data[i] = byte(i)
			}
			var b bytes.Buffer
			require.NoError(t, WriteTaskWorkspaceArchiveFrame(&b, kind, data))
			require.Equal(t, kind, b.Bytes()[0])
			require.Equal(t, uint32(n), binary.BigEndian.Uint32(b.Bytes()[1:5]))
			require.Equal(t, data, b.Bytes()[5:])
			wire := bytes.Clone(b.Bytes())
			gotKind, got, err := ReadTaskWorkspaceArchiveFrame(&b)
			require.NoError(t, err)
			require.Equal(t, kind, gotKind)
			require.Equal(t, data, got)
			for _, cut := range []int{0, 1, 4, 5, len(wire) - 1} {
				k, p, err := ReadTaskWorkspaceArchiveFrame(bytes.NewReader(wire[:cut]))
				require.Error(t, err)
				require.Zero(t, k)
				require.Nil(t, p)
			}
		}
		for _, n := range []uint32{0, uint32(maximum + 1), ^uint32(0)} {
			var h [5]byte
			h[0] = kind
			binary.BigEndian.PutUint32(h[1:], n)
			r := &archiveHeaderOnlyReader{data: h[:]}
			_, _, err := ReadTaskWorkspaceArchiveFrame(r)
			require.Error(t, err)
			require.Equal(t, 1, r.reads)
		}
		for _, n := range []int{0, maximum + 1} {
			var b bytes.Buffer
			require.Error(t, WriteTaskWorkspaceArchiveFrame(&b, kind, make([]byte, n)))
			require.Empty(t, b.Bytes())
		}
	}
	for _, kind := range []byte{0, EventAccepted, EventDiagnostics, 0xa0, 0xa3, 255} {
		var b bytes.Buffer
		require.Error(t, WriteTaskWorkspaceArchiveFrame(&b, kind, []byte{1}))
		require.Empty(t, b.Bytes())
		_, _, err := ReadTaskWorkspaceArchiveFrame(bytes.NewReader([]byte{kind, 0, 0, 0, 1}))
		require.Error(t, err)
	}
	// One frame intentionally imposes no stream order or EOF policy.
	var b bytes.Buffer
	require.NoError(t, WriteTaskWorkspaceArchiveFrame(&b, ArchiveComplete, []byte{1}))
	require.NoError(t, WriteTaskWorkspaceArchiveFrame(&b, ArchiveData, []byte{2}))
	for _, kind := range []byte{ArchiveComplete, ArchiveData} {
		got, _, err := ReadTaskWorkspaceArchiveFrame(&b)
		require.NoError(t, err)
		require.Equal(t, kind, got)
	}
	require.Equal(t, byte(1), EventAccepted)
	require.Equal(t, byte(6), EventDiagnostics)
	require.Equal(t, 32768, MaxEvent)
}

func TestTaskWorkspaceArchiveNilAndWriterErrors(t *testing.T) {
	var nilWriter *bytes.Buffer
	var nilReader *bytes.Reader
	for _, w := range []io.Writer{nil, nilWriter} {
		require.Error(t, WriteTaskWorkspaceArchiveRequest(w, archiveEnvelope()))
		require.Error(t, WriteTaskWorkspaceArchiveFrame(w, ArchiveData, []byte{1}))
	}
	for _, r := range []io.Reader{nil, nilReader} {
		_, err := ReadTaskWorkspaceArchiveRequest(r)
		require.Error(t, err)
		_, _, err = ReadTaskWorkspaceArchiveFrame(r)
		require.Error(t, err)
	}
	sentinel := errors.New("write failure")
	for _, count := range []int{-1, 0, 1, 100000} {
		for _, failAt := range []int{1, 2} {
			for _, writeErr := range []error{nil, sentinel} {
				for _, request := range []bool{false, true} {
					w := &archiveBadWriter{count: count, err: writeErr, failAt: failAt}
					var err error
					if request {
						err = WriteTaskWorkspaceArchiveRequest(w, archiveEnvelope())
					} else {
						err = WriteTaskWorkspaceArchiveFrame(w, ArchiveData, []byte{1, 2})
					}
					if writeErr != nil {
						require.ErrorIs(t, err, sentinel)
					} else {
						require.ErrorIs(t, err, io.ErrShortWrite)
					}
				}
			}
		}
	}
	_, _, err := ReadTaskWorkspaceArchiveFrame(&archiveErrorReader{})
	require.Error(t, err)
}

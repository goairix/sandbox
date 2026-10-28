package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
)

type fileV2Runtime struct {
	*mockRuntime
	mu      sync.Mutex
	calls   [][]string
	stat    string
	listing string
	stream  io.ReadCloser
	written string
}

func (r *fileV2Runtime) RunFileOp(_ context.Context, _ string, argv []string, stdin io.Reader) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), argv...))
	if r.stream != nil {
		return r.stream, nil
	}
	if argv[0] == "stat" {
		return io.NopCloser(strings.NewReader(r.stat)), nil
	}
	if argv[0] == "read" {
		return io.NopCloser(strings.NewReader("abcdef")), nil
	}
	if argv[0] == "write" {
		body, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		r.written = string(body)
		return io.NopCloser(strings.NewReader("")), nil
	}
	if argv[0] == "read-lines" {
		return io.NopCloser(strings.NewReader(`{"content":"line one\r\nline two\n"}`)), nil
	}
	if argv[0] == "edit" {
		body, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		r.written = string(body)
		return io.NopCloser(strings.NewReader(`{"replacements":1}`)), nil
	}
	return io.NopCloser(strings.NewReader(r.listing)), nil
}

func TestFilesV2TextManager(t *testing.T) {
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	mgr, _ := newGatedManager(rt)
	content, err := mgr.ReadLinesV2(context.Background(), "sandbox-test", "a.txt", 1, 0)
	require.NoError(t, err)
	require.Equal(t, "line one\r\nline two\n", content)
	require.Equal(t, []string{"read-lines", "/workspace/a.txt", "1", "0"}, rt.calls[0])
	err = mgr.EditTextV2(context.Background(), "sandbox-test", "a.txt", "line one", "new", false)
	require.NoError(t, err)
	require.Contains(t, rt.written, `"old_string":"line one"`)
	require.Equal(t, []string{"edit", "/workspace/a.txt"}, rt.calls[1])
	err = mgr.EditTextV2(context.Background(), "sandbox-test", "a.txt", "", "new", false)
	require.Error(t, err)
	require.Len(t, rt.calls, 2)
}

func TestFileV2Write(t *testing.T) {
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	mgr, _ := newGatedManager(rt)
	err := mgr.WriteFileV2(context.Background(), "sandbox-test", "nested/a.txt", 3, true, strings.NewReader("abc"))
	require.NoError(t, err)
	require.Equal(t, "abc", rt.written)
	require.Equal(t, []string{"write", "/workspace/nested/a.txt", "3", "create-parents"}, rt.calls[0])
	err = mgr.WriteFileV2(context.Background(), "sandbox-test", "/workspace/a.txt", -1, false, strings.NewReader(""))
	require.ErrorIs(t, err, runtime.ErrInvalidUploadSize)
	require.Len(t, rt.calls, 1)
}

func TestFileV2ReadRangeHoldsGateUntilClose(t *testing.T) {
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	mgr, gate := newGatedManager(rt)
	stream, err := mgr.ReadFileRangeV2(context.Background(), "sandbox-test", "/workspace/a", 2, 3)
	require.NoError(t, err)
	require.Equal(t, []string{"read", "/workspace/a", "2", "3"}, rt.calls[0])
	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	select {
	case <-drained:
		t.Fatal("operation gate drained before read stream closed")
	default:
	}
	require.NoError(t, stream.Close())
	require.NoError(t, <-drained)
	_, err = mgr.ReadFileRangeV2(context.Background(), "sandbox-test", "/workspace/a", -1, 1)
	require.Error(t, err)
}

type fileV2CloseBarrier struct {
	io.Reader
	started chan struct{}
	release chan struct{}
}

func (b *fileV2CloseBarrier) Close() error {
	close(b.started)
	<-b.release
	return nil
}

func TestFileV2StatHoldsGateThroughTransportClose(t *testing.T) {
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	rt.stream = &fileV2CloseBarrier{
		Reader:  strings.NewReader(`{"name":"a","path":"/workspace/a","size":1,"is_dir":false,"mod_time":"2026-01-01T00:00:00Z"}`),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	mgr, gate := newGatedManager(rt)
	result := make(chan error, 1)
	go func() { _, err := mgr.StatFileV2(context.Background(), "sandbox-test", "/workspace/a"); result <- err }()
	select {
	case <-rt.stream.(*fileV2CloseBarrier).started:
	case <-time.After(time.Second):
		t.Fatal("transport Close was not called")
	}
	drained := make(chan error, 1)
	go func() { drained <- gate.CloseAndWait(context.Background()) }()
	select {
	case <-drained:
		t.Fatal("gate drained before transport Close completed")
	default:
	}
	close(rt.stream.(*fileV2CloseBarrier).release)
	require.NoError(t, <-result)
	require.NoError(t, <-drained)
}

func TestFileV2StatList(t *testing.T) {
	reserved := fuseprotocol.ProbeObjectBasenamePrefix + strings.Repeat("a", 64)
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	stat, err := json.Marshal(map[string]any{
		"name": "a.txt", "path": "/workspace/a.txt", "size": 7, "is_dir": false,
		"mod_time": "2026-01-01T00:00:00Z",
	})
	require.NoError(t, err)
	listing, err := json.Marshal(map[string]any{
		"files": []map[string]any{
			{"name": "!.txt", "path": "/workspace/!.txt", "size": 7, "is_dir": false, "mod_time": "2026-01-01T00:00:00Z"},
			{"name": "a.txt", "path": "/workspace/a.txt", "size": 1, "is_dir": false, "mod_time": "2026-01-01T00:00:00Z"},
		},
		"next_cursor": "/workspace/a.txt",
	})
	require.NoError(t, err)
	rt.stat, rt.listing = string(stat), string(listing)
	mgr, _ := newGatedManager(rt)

	info, err := mgr.StatFileV2(context.Background(), "sandbox-test", "/workspace/a.txt")
	require.NoError(t, err)
	require.Equal(t, runtime.FileInfo{Name: "a.txt", Path: "/workspace/a.txt", Size: 7, ModTime: info.ModTime}, info)

	page, err := mgr.ListFilesV2(context.Background(), "sandbox-test", "/workspace", "", 0)
	require.NoError(t, err)
	require.Len(t, page.Files, 2)
	require.Equal(t, "/workspace/!.txt", page.Files[0].Path)
	require.Equal(t, "/workspace/a.txt", page.NextCursor)
	require.Equal(t, []string{"list", "/workspace", "", "200", "hide-reserved"}, rt.calls[1])
	rt.listing = `{"files":[{"name":"` + reserved + `","path":"/workspace/` + reserved + `","size":1,"is_dir":false,"mod_time":"2026-01-01T00:00:00Z"}],"next_cursor":"/workspace/` + reserved + `"}`
	_, err = mgr.ListFilesV2(context.Background(), "sandbox-test", "/workspace", "", 1)
	require.ErrorContains(t, err, "reserved cursor")

	_, err = mgr.ListFilesV2(context.Background(), "sandbox-test", "/workspace", "", 1001)
	require.Error(t, err)
	require.Len(t, rt.calls, 3, "invalid limit reached runtime")
	_, err = mgr.StatFileV2(context.Background(), "sandbox-test", "/workspace/"+reserved)
	require.ErrorIs(t, err, ErrReservedFUSEWorkspacePath)
	require.Len(t, rt.calls, 3, "reserved file reached runtime")
}

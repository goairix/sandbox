# Workspace HTML Content-Disposition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist `Content-Disposition: inline` for HTML workspace files during both full and incremental synchronization to object storage.

**Architecture:** Replace the single Content-Type option helper with one helper that constructs all storage-write options. It always sets Content-Type and conditionally appends Content-Disposition for case-insensitive `.html` and `.htm` extensions; both sync paths consume the same helper.

**Tech Stack:** Go 1.25, `github.com/goairix/fs v0.3.11`, Testify

---

## File Structure

- Modify `internal/sandbox/workspace.go`: construct storage-write options and use them in full and incremental sync paths.
- Modify `internal/sandbox/workspace_test.go`: verify MIME type and Content-Disposition option behavior independently of a concrete storage backend.
- Modify `go.mod`: retain the existing user-provided upgrade to `github.com/goairix/fs v0.3.11` and its resolved indirect dependencies.
- Modify `go.sum`: retain checksums produced by the dependency upgrade.

### Task 1: Add HTML storage-write options

**Files:**
- Modify: `internal/sandbox/workspace_test.go`
- Modify: `internal/sandbox/workspace.go:23-30`
- Modify: `internal/sandbox/workspace.go:640`
- Modify: `internal/sandbox/workspace.go:718`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Write the failing option-construction test**

Add this test after `TestIsExcluded` in `internal/sandbox/workspace_test.go`:

```go
func TestStorageWriteOptions(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		wantContentType string
		wantDisposition string
	}{
		{name: "html", path: "index.html", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "htm", path: "page.htm", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "mixed case html", path: "report.HtMl", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "plain text", path: "notes.txt", wantContentType: "text/plain; charset=utf-8"},
		{name: "unknown extension", path: "payload.unknownext", wantContentType: "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := &fs.Options{}
			for _, opt := range storageWriteOptions(tt.path) {
				opt(got)
			}

			assert.Equal(t, tt.wantContentType, got.ContentType)
			assert.Equal(t, tt.wantDisposition, got.ContentDisposition)
		})
	}
}
```

- [ ] **Step 2: Run the focused test and verify that it fails**

Run:

```bash
go test ./internal/sandbox -run '^TestStorageWriteOptions$' -count=1
```

Expected: compilation fails with `undefined: storageWriteOptions`, proving the test exercises behavior that does not yet exist.

- [ ] **Step 3: Implement the storage-write option helper**

Replace `contentTypeOpt` in `internal/sandbox/workspace.go` with:

```go
// storageWriteOptions returns object-storage metadata based on the file extension.
func storageWriteOptions(name string) []fs.Option {
	ext := strings.ToLower(filepath.Ext(name))
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "application/octet-stream"
	}

	opts := []fs.Option{fs.WithContentType(ct)}
	if ext == ".html" || ext == ".htm" {
		opts = append(opts, fs.WithContentDisposition("inline"))
	}
	return opts
}
```

The file already imports `strings`, so this implementation requires no new import.

- [ ] **Step 4: Apply the helper to both storage-write paths**

In `fullSyncFromContainer`, replace:

```go
writer, err := scoped.Create(ctx, name, contentTypeOpt(name))
```

with:

```go
writer, err := scoped.Create(ctx, name, storageWriteOptions(name)...)
```

In `downloadChangedFiles`, make the identical replacement:

```go
writer, err := scoped.Create(ctx, name, storageWriteOptions(name)...)
```

- [ ] **Step 5: Run the focused test and verify that it passes**

Run:

```bash
go test ./internal/sandbox -run '^TestStorageWriteOptions$' -count=1
```

Expected: `ok github.com/goairix/sandbox/internal/sandbox`.

- [ ] **Step 6: Run all sandbox package tests**

Run:

```bash
go test ./internal/sandbox/... -count=1
```

Expected: all packages report `ok` or `[no test files]`, with no failures.

- [ ] **Step 7: Verify formatting and the complete repository test suite**

Run:

```bash
gofmt -w internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git diff --check
go test ./...
```

Expected: `git diff --check` produces no output and every Go package passes.

- [ ] **Step 8: Commit the dependency upgrade and implementation together**

Run:

```bash
git add go.mod go.sum internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "fix(workspace): set HTML content disposition on sync"
```

Expected: the commit contains only the `fs v0.3.11` dependency upgrade, the shared option helper, its two call-site updates, and the focused tests.

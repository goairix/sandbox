# Batch File Download Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add GlobInfo and DownloadFiles methods to Runtime interface to enable batch file downloads, reducing ListSkills from N I/O operations to 1.

**Architecture:** Extend Runtime interface with two new methods (GlobInfo for pattern matching, DownloadFiles for explicit paths). Implement in Kubernetes runtime using parallel downloads. Add Manager wrappers and update ListSkills handler to use GlobInfo.

**Tech Stack:** Go, Kubernetes client-go, tar archives, goroutines

---

## File Structure

**New files:**
- None (extending existing interfaces)

**Modified files:**
- `internal/runtime/runtime.go` - Add FileContent type and new interface methods
- `internal/runtime/kubernetes/file.go` - Implement GlobInfo and DownloadFiles
- `internal/runtime/docker/file.go` - Implement GlobInfo and DownloadFiles
- `internal/sandbox/manager.go` - Add Manager wrapper methods
- `internal/api/handler/skill.go` - Refactor ListSkills to use GlobInfo

---

### Task 1: Add FileContent Type and Runtime Interface Methods

**Files:**
- Modify: `internal/runtime/runtime.go:87-111`

- [ ] **Step 1: Add FileContent type definition**

Add after line 111 in `internal/runtime/runtime.go`:

```go
// FileContent represents a file with its content stream.
type FileContent struct {
	Path    string
	Content io.ReadCloser // tar stream for single file
	Error   error         // per-file error (for batch operations)
}
```

- [ ] **Step 2: Add GlobInfo method to Runtime interface**

Add after line 61 in `internal/runtime/runtime.go`:

```go
// GlobInfo returns files matching the glob pattern with their content.
// Pattern syntax: "*/*.md" matches all .md files in immediate subdirectories.
// Returns FileContent slice where each Content is a tar stream (same format as DownloadFile).
GlobInfo(ctx context.Context, id string, pattern string) ([]FileContent, error)
```

- [ ] **Step 3: Add DownloadFiles method to Runtime interface**

Add after GlobInfo in `internal/runtime/runtime.go`:

```go
// DownloadFiles downloads multiple files in parallel.
// Returns partial results even if some files fail (check FileContent.Error).
// Each Content is a tar stream (same format as DownloadFile).
DownloadFiles(ctx context.Context, id string, paths []string) ([]FileContent, error)
```

- [ ] **Step 4: Verify compilation**

Run: `go build ./internal/runtime/...`
Expected: Compilation errors about missing methods in kubernetes and docker runtimes

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/runtime.go
git commit -m "feat: add FileContent type and batch download methods to Runtime interface"
```

---

### Task 2: Implement GlobInfo for Kubernetes Runtime

**Files:**
- Modify: `internal/runtime/kubernetes/file.go:103`

- [ ] **Step 1: Add GlobInfo implementation**

Add after line 102 in `internal/runtime/kubernetes/file.go`:

```go
func (r *Runtime) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	// Extract base directory and glob pattern
	lastSlash := strings.LastIndex(pattern, "/")
	if lastSlash == -1 {
		return nil, fmt.Errorf("invalid pattern: must contain directory path")
	}
	baseDir := pattern[:lastSlash]
	globPattern := pattern[lastSlash+1:]

	// Execute find command to get matching files
	execReq := client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(id).
		Namespace(r.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   []string{"sh", "-c", fmt.Sprintf("cd %s && find . -name '%s' -type f", baseDir, globPattern)},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.restConfig, "POST", execReq.URL())
	if err != nil {
		return nil, fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("find command failed: %w, stderr: %s", err, stderr.String())
	}

	// Parse file paths from find output
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []runtime.FileContent{}, nil
	}

	// Download each file in parallel
	results := make([]runtime.FileContent, len(lines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // Limit concurrency to 10

	for i, line := range lines {
		wg.Add(1)
		go func(idx int, relPath string) {
			defer wg.Done()
			sem <- struct{}
			defer func() { <-sem }()

			fullPath := filepath.Join(baseDir, strings.TrimPrefix(relPath, "./"))
			reader, err := downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, fullPath)
			results[idx] = runtime.FileContent{
				Path:    fullPath,
				Content: reader,
				Error:   err,
			}
		}(i, line)
	}
	wg.Wait()

	return results, nil
}
```

- [ ] **Step 2: Add required imports**

Add to imports in `internal/runtime/kubernetes/file.go`:

```go
"bytes"
"path/filepath"
"strings"
"sync"
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./internal/runtime/kubernetes/...`
Expected: SUCCESS

- [ ] **Step 4: Commit**

```bash
git add internal/runtime/kubernetes/file.go
git commit -m "feat(k8s): implement GlobInfo for batch file downloads"
```

---

### Task 3: Implement DownloadFiles for Kubernetes Runtime

**Files:**
- Modify: `internal/runtime/kubernetes/file.go`

- [ ] **Step 1: Add DownloadFiles implementation**

Add after GlobInfo in `internal/runtime/kubernetes/file.go`:

```go
func (r *Runtime) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
	results := make([]runtime.FileContent, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // Limit concurrency to 10

	for i, path := range paths {
		wg.Add(1)
		go func(idx int, filePath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reader, err := downloadFileFromPod(ctx, r.client, r.restConfig, r.namespace, id, filePath)
			results[idx] = runtime.FileContent{
				Path:    filePath,
				Content: reader,
				Error:   err,
			}
		}(i, path)
	}
	wg.Wait()

	return results, nil
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./internal/runtime/kubernetes/...`
Expected: SUCCESS

- [ ] **Step 3: Commit**

```bash
git add internal/runtime/kubernetes/file.go
git commit -m "feat(k8s): implement DownloadFiles for parallel batch downloads"
```

---

### Task 4: Implement GlobInfo and DownloadFiles for Docker Runtime

**Files:**
- Modify: `internal/runtime/docker/file.go`

- [ ] **Step 1: Add GlobInfo implementation**

Add to `internal/runtime/docker/file.go`:

```go
func (r *Runtime) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	lastSlash := strings.LastIndex(pattern, "/")
	if lastSlash == -1 {
		return nil, fmt.Errorf("invalid pattern: must contain directory path")
	}
	baseDir := pattern[:lastSlash]
	globPattern := pattern[lastSlash+1:]

	execResp, err := r.client.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          []string{"sh", "-c", fmt.Sprintf("cd %s && find . -name '%s' -type f", baseDir, globPattern)},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	attachResp, err := r.client.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	defer attachResp.Close()

	var stdout bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, io.Discard, attachResp.Reader); err != nil {
		return nil, fmt.Errorf("read exec output: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []runtime.FileContent{}, nil
	}

	results := make([]runtime.FileContent, len(lines))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i, line := range lines {
		wg.Add(1)
		go func(idx int, relPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fullPath := filepath.Join(baseDir, strings.TrimPrefix(relPath, "./"))
			reader, err := r.downloadFile(ctx, id, fullPath)
			results[idx] = runtime.FileContent{
				Path:    fullPath,
				Content: reader,
				Error:   err,
			}
		}(i, line)
	}
	wg.Wait()

	return results, nil
}
```

- [ ] **Step 2: Add DownloadFiles implementation**

Add after GlobInfo in `internal/runtime/docker/file.go`:

```go
func (r *Runtime) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
	results := make([]runtime.FileContent, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)

	for i, path := range paths {
		wg.Add(1)
		go func(idx int, filePath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			reader, err := r.downloadFile(ctx, id, filePath)
			results[idx] = runtime.FileContent{
				Path:    filePath,
				Content: reader,
				Error:   err,
			}
		}(i, path)
	}
	wg.Wait()

	return results, nil
}
```

- [ ] **Step 3: Extract downloadFile helper method**

Add helper method in `internal/runtime/docker/file.go`:

```go
func (r *Runtime) downloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	execResp, err := r.client.ContainerExecCreate(ctx, id, types.ExecConfig{
		Cmd:          []string{"tar", "cf", "-", srcPath},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}

	attachResp, err := r.client.ContainerExecAttach(ctx, execResp.ID, types.ExecStartCheck{})
	if err != nil {
		return nil, fmt.Errorf("attach exec: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, io.Discard, attachResp.Reader)
		attachResp.Close()
		pw.CloseWithError(err)
	}()

	return pr, nil
}
```

- [ ] **Step 4: Verify compilation**

Run: `go build ./internal/runtime/docker/...`
Expected: SUCCESS

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/docker/file.go
git commit -m "feat(docker): implement GlobInfo and DownloadFiles"
```

---

### Task 5: Add Manager Wrapper Methods

**Files:**
- Modify: `internal/sandbox/manager.go:506`

- [ ] **Step 1: Add GlobInfo wrapper**

Add after line 506 in `internal/sandbox/manager.go`:

```go
// GlobInfo returns files matching the glob pattern with their content.
func (m *Manager) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.GlobInfo",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	return m.runtime.GlobInfo(ctx, sb.RuntimeID, pattern)
}
```

- [ ] **Step 2: Add DownloadFiles wrapper**

Add after GlobInfo in `internal/sandbox/manager.go`:

```go
// DownloadFiles downloads multiple files in parallel.
func (m *Manager) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.DownloadFiles",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	return m.runtime.DownloadFiles(ctx, sb.RuntimeID, paths)
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./internal/sandbox/...`
Expected: SUCCESS

- [ ] **Step 4: Commit**

```bash
git add internal/sandbox/manager.go
git commit -m "feat: add GlobInfo and DownloadFiles wrappers to Manager"
```

---

### Task 6: Refactor ListSkills to Use GlobInfo

**Files:**
- Modify: `internal/api/handler/skill.go:110-163`

- [ ] **Step 1: Add extractSkillMeta helper method**

Add after loadSkillMeta in `internal/api/handler/skill.go`:

```go
// extractSkillMeta extracts skill metadata from a tar stream.
func (h *Handler) extractSkillMeta(path string, tarReader io.ReadCloser) types.SkillMeta {
	defer tarReader.Close()

	// Extract skill name from path: /workspace/.agent/skills/<name>/SKILL.md
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return types.SkillMeta{Name: "unknown"}
	}
	skillName := parts[len(parts)-2]

	tr := tar.NewReader(tarReader)
	if _, err := tr.Next(); err != nil {
		return types.SkillMeta{Name: skillName}
	}

	raw, err := io.ReadAll(tr)
	if err != nil {
		return types.SkillMeta{Name: skillName}
	}

	meta, _, err := ParseFrontmatter(string(raw))
	if err != nil {
		return types.SkillMeta{Name: skillName}
	}

	if meta.Name == "" {
		meta.Name = skillName
	}
	return meta
}
```

- [ ] **Step 2: Refactor ListSkills to use GlobInfo**

Replace ListSkills function (lines 110-163) in `internal/api/handler/skill.go`:

```go
// ListSkills handles GET /api/v1/sandboxes/:id/skills
func (h *Handler) ListSkills(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.skill.ListSkills")
	defer span.End()

	id := c.Param("id")

	if _, err := h.manager.Get(spanCtx, id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	// Use GlobInfo to fetch all SKILL.md files in one call
	pattern := skillsBasePath + "/*/SKILL.md"
	files, err := h.manager.GlobInfo(spanCtx, id, pattern)
	if err != nil {
		c.JSON(http.StatusOK, types.SkillListResponse{Skills: []types.SkillMeta{}})
		return
	}

	skills := make([]types.SkillMeta, 0, len(files))
	for _, file := range files {
		if file.Error != nil {
			logger.Warn(spanCtx, "failed to download skill file",
				logger.AddField("path", file.Path),
				logger.ErrorField(file.Error))
			continue
		}
		meta := h.extractSkillMeta(file.Path, file.Content)
		skills = append(skills, meta)
	}

	c.JSON(http.StatusOK, types.SkillListResponse{Skills: skills})
}
```

- [ ] **Step 3: Remove loadSkillMeta method**

Delete the loadSkillMeta method (no longer needed).

- [ ] **Step 4: Verify compilation**

Run: `go build ./internal/api/...`
Expected: SUCCESS

- [ ] **Step 5: Commit**

```bash
git add internal/api/handler/skill.go
git commit -m "refactor: use GlobInfo in ListSkills for batch downloads"
```

---

### Task 7: Manual Testing

**Files:**
- None (testing only)

- [ ] **Step 1: Start sandbox service**

Run: `go run cmd/sandbox/main.go`
Expected: Service starts successfully

- [ ] **Step 2: Create test sandbox with skills**

```bash
curl -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"language": "python"}'
```

Expected: Returns sandbox ID

- [ ] **Step 3: Upload test skill files**

```bash
# Create test skill structure
mkdir -p /tmp/test-skills/skill1
echo "---
name: test-skill-1
description: Test skill
---
Content" > /tmp/test-skills/skill1/SKILL.md

# Upload to sandbox (use sandbox ID from step 2)
curl -X POST http://localhost:8080/api/v1/sandboxes/{id}/files \
  -F "file=@/tmp/test-skills/skill1/SKILL.md" \
  -F "path=/workspace/.agent/skills/skill1/SKILL.md"
```

- [ ] **Step 4: Test ListSkills API**

```bash
curl http://localhost:8080/api/v1/sandboxes/{id}/skills
```

Expected: Returns skill list with test-skill-1

- [ ] **Step 5: Verify logs show single I/O operation**

Check logs for GlobInfo call (not multiple DownloadFile calls).

---

### Task 8: Final Commit and Cleanup

**Files:**
- Modify: `docs/superpowers/specs/2026-04-29-batch-file-download-design.md`

- [ ] **Step 1: Update spec with implementation notes**

Add "Implementation Status" section at end of spec:

```markdown
## Implementation Status

✅ Completed 2026-04-29
- FileContent type added to runtime package
- GlobInfo and DownloadFiles implemented for Kubernetes and Docker runtimes
- Manager wrappers added
- ListSkills refactored to use GlobInfo
- Manual testing verified single I/O operation
```

- [ ] **Step 2: Final commit**

```bash
git add docs/superpowers/specs/2026-04-29-batch-file-download-design.md
git commit -m "docs: mark batch file download implementation as complete"
```

- [ ] **Step 3: Push changes**

```bash
git push origin main
```

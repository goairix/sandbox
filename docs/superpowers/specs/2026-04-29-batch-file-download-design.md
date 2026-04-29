# Batch File Download Design

## Overview

Add batch file download capabilities to optimize ListSkills API performance. Currently, ListSkills makes N separate DownloadFile calls (one per skill), causing N file I/O operations. This design introduces two new methods: GlobInfo for pattern-based batch downloads and DownloadFiles for explicit path list downloads.

## Problem Statement

The ListSkills handler iterates through skill directories and downloads each SKILL.md file individually:

```go
for _, entry := range entries {
    tarReader, err := h.manager.DownloadFile(ctx, id, skillMDPath)  // N I/O operations
    // ...
}
```

This results in N round-trips to the runtime layer, each executing a separate tar command in the sandbox container.

## Goals

1. Reduce ListSkills from N I/O operations to 1
2. Provide reusable batch download primitives for other use cases
3. Maintain backward compatibility with existing DownloadFile
4. Keep memory footprint reasonable (streaming, not buffering all files)

## Architecture

### Type Definitions

Add to `internal/runtime/runtime.go`:

```go
// FileContent represents a file with its content stream.
type FileContent struct {
    Path    string
    Content io.ReadCloser  // tar stream for single file
    Error   error          // per-file error (for batch operations)
}
```

### Runtime Interface

Extend `Runtime` interface in `internal/runtime/runtime.go`:

```go
// GlobInfo returns files matching the glob pattern with their content.
// Pattern syntax: "*/*.md" matches all .md files in immediate subdirectories.
// Returns FileContent slice where each Content is a tar stream (same format as DownloadFile).
GlobInfo(ctx context.Context, id string, pattern string) ([]FileContent, error)

// DownloadFiles downloads multiple files in parallel.
// Returns partial results even if some files fail (check FileContent.Error).
// Each Content is a tar stream (same format as DownloadFile).
DownloadFiles(ctx context.Context, id string, paths []string) ([]FileContent, error)
```

### Implementation Strategy

**GlobInfo (Kubernetes):**
- Execute `find <baseDir> -path '<pattern>' -type f` to get matching paths
- For each path, call existing `downloadFileFromPod` helper
- Return slice of FileContent with tar streams

**DownloadFiles (Kubernetes):**
- Use goroutines to call `downloadFileFromPod` in parallel (limit concurrency to 10)
- Collect results with per-file error handling
- Return partial results even if some downloads fail

**Docker implementations:** Similar approach using Docker exec API.

### Manager Layer

Add wrapper methods to `internal/sandbox/manager.go`:

```go
func (m *Manager) GlobInfo(ctx context.Context, id string, pattern string) ([]runtime.FileContent, error) {
    sb, err := m.resolve(ctx, id)
    if err != nil {
        return nil, err
    }
    return m.runtime.GlobInfo(ctx, sb.RuntimeID, pattern)
}

func (m *Manager) DownloadFiles(ctx context.Context, id string, paths []string) ([]runtime.FileContent, error) {
    sb, err := m.resolve(ctx, id)
    if err != nil {
        return nil, err
    }
    return m.runtime.DownloadFiles(ctx, sb.RuntimeID, paths)
}
```

### Handler Updates

Update `internal/api/handler/skill.go`:

```go
func (h *Handler) ListSkills(c *gin.Context) {
    // ... validation ...

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
            continue
        }
        meta := h.extractSkillMeta(file.Path, file.Content)
        skills = append(skills, meta)
    }

    c.JSON(http.StatusOK, types.SkillListResponse{Skills: skills})
}

func (h *Handler) extractSkillMeta(path string, tarReader io.ReadCloser) types.SkillMeta {
    defer tarReader.Close()
    
    // Extract skill name from path: /workspace/.agent/skills/<name>/SKILL.md
    parts := strings.Split(path, "/")
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

## Error Handling

- **GlobInfo**: Returns error only for catastrophic failures (exec failure, invalid pattern). Empty result set is valid.
- **DownloadFiles**: Returns partial results. Check `FileContent.Error` for per-file failures.
- **Handler**: Gracefully degrades - skips files with errors, returns available skills.

## Testing Strategy

1. Unit tests for glob pattern matching
2. Integration tests for batch download with mixed success/failure
3. Performance test comparing N individual calls vs 1 batch call
4. Edge cases: empty results, all files fail, pattern with no matches

## Performance Impact

**Before:** N file I/O operations for N skills
**After:** 1 file I/O operation (glob + batch download)

Expected improvement: ~10x faster for 10 skills, scales linearly.

## Backward Compatibility

- Existing `DownloadFile` remains unchanged
- New methods are additive, no breaking changes
- Handlers can adopt batch methods incrementally

## Future Enhancements

- Add caching layer for frequently accessed skill metadata
- Support recursive glob patterns (`**/*.md`)
- Add batch upload counterpart (`UploadFiles`)

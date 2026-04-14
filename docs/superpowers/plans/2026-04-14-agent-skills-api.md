# Agent Skills API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add three read-only HTTP endpoints so AI agents can discover and read skills stored at `/workspace/.agent/skills/` inside a sandbox.

**Architecture:** New `skill.go` files in `pkg/types/` and `internal/api/handler/` handle types and request logic respectively. Handlers reuse the existing `*sandbox.Manager` (via `h.manager`) for file I/O — no new dependencies. Three routes are registered in `router.go` under the existing `v1` group.

**Tech Stack:** Go, Gin, `github.com/goccy/go-yaml` (already in go.mod), existing `h.manager.ListFiles` / `h.manager.DownloadFile`

---

### Task 1: Add skill types

**Files:**
- Create: `pkg/types/skill.go`

- [ ] **Step 1: Write the file**

```go
package types

// SkillMeta holds the parsed YAML frontmatter fields of a SKILL.md.
type SkillMeta struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// SkillFile describes a non-SKILL.md file inside a skill directory.
type SkillFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SkillListResponse is returned by GET /sandboxes/:id/skills.
type SkillListResponse struct {
	Skills []SkillMeta `json:"skills"`
}

// SkillResponse is returned by GET /sandboxes/:id/skills/:name.
type SkillResponse struct {
	SkillMeta
	Content string      `json:"content"`
	Files   []SkillFile `json:"files"`
}
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./pkg/types/...
```

Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add pkg/types/skill.go
git commit -m "feat(types): add Skill types for agent skills API"
```

---

### Task 2: Add skill handler

**Files:**
- Create: `internal/api/handler/skill.go`

- [ ] **Step 1: Write the failing test**

Create `internal/api/handler/skill_test.go`:

```go
package handler_test

import (
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/api/handler"
)

func TestParseFrontmatter_Valid(t *testing.T) {
	input := "---\nname: my-skill\ndescription: Does things\ncompatibility: needs curl\nmetadata:\n  author: alice\n  version: \"1.0\"\n---\n\n# Body here\n"
	meta, body, err := handler.ParseFrontmatter(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Name != "my-skill" {
		t.Errorf("name: got %q, want %q", meta.Name, "my-skill")
	}
	if meta.Description != "Does things" {
		t.Errorf("description: got %q", meta.Description)
	}
	if meta.Compatibility != "needs curl" {
		t.Errorf("compatibility: got %q", meta.Compatibility)
	}
	if meta.Metadata["author"] != "alice" {
		t.Errorf("metadata.author: got %q", meta.Metadata["author"])
	}
	if !strings.Contains(body, "# Body here") {
		t.Errorf("body missing content: %q", body)
	}
}

func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	input := "# Just a body\nno frontmatter here\n"
	meta, body, err := handler.ParseFrontmatter(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Name != "" {
		t.Errorf("expected empty name, got %q", meta.Name)
	}
	if body != input {
		t.Errorf("body should equal input when no frontmatter")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/api/handler/... -run TestParseFrontmatter -v
```

Expected: FAIL — `handler.ParseFrontmatter` undefined.

- [ ] **Step 3: Write the handler**

Create `internal/api/handler/skill.go`:

```go
package handler

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"

	"github.com/goairix/sandbox/pkg/types"
)

const skillsBasePath = "/workspace/.agent/skills"

// ParseFrontmatter splits a SKILL.md string into structured metadata and body.
// If no frontmatter block is found, meta is zero-value and body equals the full input.
func ParseFrontmatter(content string) (types.SkillMeta, string, error) {
	var meta types.SkillMeta
	if !strings.HasPrefix(content, "---\n") {
		return meta, content, nil
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---\n")
	if end == -1 {
		return meta, content, nil
	}
	frontmatter := rest[:end]
	body := rest[end+5:]

	// Parse frontmatter YAML into a raw map first to handle metadata sub-map.
	var raw struct {
		Name          string            `yaml:"name"`
		Description   string            `yaml:"description"`
		Compatibility string            `yaml:"compatibility"`
		Metadata      map[string]string `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(frontmatter), &raw); err != nil {
		return meta, body, fmt.Errorf("parse frontmatter: %w", err)
	}
	meta.Name = raw.Name
	meta.Description = raw.Description
	meta.Compatibility = raw.Compatibility
	meta.Metadata = raw.Metadata
	return meta, body, nil
}

// ListSkills handles GET /api/v1/sandboxes/:id/skills
func (h *Handler) ListSkills(c *gin.Context) {
	id := c.Param("id")

	if _, err := h.manager.Get(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	entries, err := h.manager.ListFiles(c.Request.Context(), id, skillsBasePath)
	if err != nil {
		// Skills directory doesn't exist — return empty list.
		c.JSON(http.StatusOK, types.SkillListResponse{Skills: []types.SkillMeta{}})
		return
	}

	var skills []types.SkillMeta
	for _, entry := range entries {
		if !entry.IsDir {
			continue
		}
		skillMDPath := skillsBasePath + "/" + entry.Name + "/SKILL.md"
		reader, err := h.manager.DownloadFile(c.Request.Context(), id, skillMDPath)
		if err != nil {
			// SKILL.md missing — include with name only.
			skills = append(skills, types.SkillMeta{Name: entry.Name})
			continue
		}
		raw, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			skills = append(skills, types.SkillMeta{Name: entry.Name})
			continue
		}
		meta, _, parseErr := ParseFrontmatter(string(raw))
		if parseErr != nil || meta.Name == "" {
			meta.Name = entry.Name
		}
		skills = append(skills, meta)
	}

	c.JSON(http.StatusOK, types.SkillListResponse{Skills: skills})
}

// GetSkill handles GET /api/v1/sandboxes/:id/skills/:name
func (h *Handler) GetSkill(c *gin.Context) {
	id := c.Param("id")
	name := c.Param("name")

	if _, err := h.manager.Get(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	skillMDPath := skillsBasePath + "/" + name + "/SKILL.md"
	reader, err := h.manager.DownloadFile(c.Request.Context(), id, skillMDPath)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: "skill not found"})
		return
	}
	raw, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	meta, body, _ := ParseFrontmatter(string(raw))
	if meta.Name == "" {
		meta.Name = name
	}

	// List all non-SKILL.md files in the skill directory (recursive).
	skillDir := skillsBasePath + "/" + name
	allEntries, err := h.manager.ListFiles(c.Request.Context(), id, skillDir)
	var skillFiles []types.SkillFile
	if err == nil {
		for _, f := range allEntries {
			if f.IsDir || f.Name == "SKILL.md" {
				continue
			}
			skillFiles = append(skillFiles, types.SkillFile{
				Name: f.Name,
				Path: f.Path,
			})
		}
	}

	c.JSON(http.StatusOK, types.SkillResponse{
		SkillMeta: meta,
		Content:   body,
		Files:     skillFiles,
	})
}

// GetSkillFile handles GET /api/v1/sandboxes/:id/skills/:name/files/*filepath
func (h *Handler) GetSkillFile(c *gin.Context) {
	id := c.Param("id")
	name := c.Param("name")
	relPath := c.Param("filepath") // includes leading slash from wildcard

	if _, err := h.manager.Get(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	// Prevent path traversal: clean and ensure it stays within the skill dir.
	cleaned := filepath.Clean(relPath)
	if strings.Contains(cleaned, "..") {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid path"})
		return
	}

	fullPath := skillsBasePath + "/" + name + "/" + strings.TrimPrefix(cleaned, "/")
	reader, err := h.manager.DownloadFile(c.Request.Context(), id, fullPath)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: "file not found"})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q", filepath.Base(fullPath)))
	if _, err := io.Copy(c.Writer, reader); err != nil {
		// Response already started; nothing useful to do.
		_ = err
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/api/handler/... -run TestParseFrontmatter -v
```

Expected: PASS for both `TestParseFrontmatter_Valid` and `TestParseFrontmatter_NoFrontmatter`.

- [ ] **Step 5: Verify full package compiles**

```bash
go build ./internal/api/handler/...
```

Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add internal/api/handler/skill.go internal/api/handler/skill_test.go
git commit -m "feat(handler): add ListSkills, GetSkill, GetSkillFile handlers"
```

---

### Task 3: Register routes

**Files:**
- Modify: `internal/api/router.go`

- [ ] **Step 1: Add the three routes**

In `internal/api/router.go`, after the workspace operations block (line 63), add:

```go
	// Agent skill discovery
	v1.GET("/sandboxes/:id/skills", h.ListSkills)
	v1.GET("/sandboxes/:id/skills/:name", h.GetSkill)
	v1.GET("/sandboxes/:id/skills/:name/files/*filepath", h.GetSkillFile)
```

- [ ] **Step 2: Build to verify no conflicts**

```bash
go build ./...
```

Expected: no output.

- [ ] **Step 3: Commit**

```bash
git add internal/api/router.go
git commit -m "feat(router): register agent skills API routes"
```

---

### Task 4: Integration smoke test

This task verifies the full flow against a running sandbox service. Run manually after `go run ./cmd/sandbox` is started with a valid config.

- [ ] **Step 1: Create a test sandbox**

```bash
SB=$(curl -s -X POST http://localhost:8080/api/v1/sandboxes \
  -H "Authorization: Bearer $SANDBOX_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"mode":"ephemeral"}' | jq -r '.id')
echo "sandbox: $SB"
```

Expected: prints a sandbox ID like `sandbox-abc123`.

- [ ] **Step 2: Create a skill directory and SKILL.md inside the sandbox**

```bash
curl -s -X POST "http://localhost:8080/api/v1/sandboxes/$SB/exec" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "command": "mkdir -p /workspace/.agent/skills/test-skill/scripts && printf -- \"---\\nname: test-skill\\ndescription: A test skill\\n---\\n\\n# Test Skill\\nDoes nothing.\\n\" > /workspace/.agent/skills/test-skill/SKILL.md && echo hello > /workspace/.agent/skills/test-skill/scripts/run.sh"
  }' | jq .
```

Expected: `{"exit_code":0,...}`.

- [ ] **Step 3: List skills**

```bash
curl -s "http://localhost:8080/api/v1/sandboxes/$SB/skills" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" | jq .
```

Expected:
```json
{
  "skills": [
    {
      "name": "test-skill",
      "description": "A test skill"
    }
  ]
}
```

- [ ] **Step 4: Get skill detail**

```bash
curl -s "http://localhost:8080/api/v1/sandboxes/$SB/skills/test-skill" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" | jq .
```

Expected: response includes `"content": "# Test Skill\nDoes nothing.\n"` and `"files"` array containing `run.sh`.

- [ ] **Step 5: Get attached file**

```bash
curl -s "http://localhost:8080/api/v1/sandboxes/$SB/skills/test-skill/files/scripts/run.sh" \
  -H "Authorization: Bearer $SANDBOX_API_KEY"
```

Expected: prints `hello`.

- [ ] **Step 6: Verify 404 for unknown skill**

```bash
curl -s -o /dev/null -w "%{http_code}" \
  "http://localhost:8080/api/v1/sandboxes/$SB/skills/no-such-skill" \
  -H "Authorization: Bearer $SANDBOX_API_KEY"
```

Expected: `404`.

- [ ] **Step 7: Destroy sandbox**

```bash
curl -s -X DELETE "http://localhost:8080/api/v1/sandboxes/$SB" \
  -H "Authorization: Bearer $SANDBOX_API_KEY" | jq .
```

- [ ] **Step 8: Commit smoke test notes (optional)**

No code changes in this task — skip commit if nothing was modified.

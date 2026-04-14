# Agent Skills API Design

**Date:** 2026-04-14

## Overview

Add a read-only HTTP API for AI agents to discover and read skills stored in a sandbox workspace. Skills live at `/workspace/.agent/skills/{name}/SKILL.md` within each sandbox. The API follows a two-step pattern: list available skills (name + metadata), then fetch full content on demand.

## Endpoints

### 1. List Skills

```
GET /api/v1/sandboxes/:id/skills
```

Reads `/workspace/.agent/skills/` in the specified sandbox, enumerates subdirectories, parses the YAML frontmatter from each `SKILL.md`, and returns structured metadata.

**Response:**
```json
{
  "skills": [
    {
      "name": "sandbox-execute",
      "description": "Execute code in a secure sandbox...",
      "compatibility": "Requires a running Sandbox API service...",
      "metadata": {
        "author": "goairix",
        "version": "2.0"
      }
    }
  ]
}
```

Frontmatter parse failures are handled gracefully: `name` falls back to the directory name, other fields are empty strings/nil. The skill is still included in the list.

### 2. Get Skill

```
GET /api/v1/sandboxes/:id/skills/:name
```

Returns the parsed frontmatter plus the full body of `SKILL.md` (everything after the closing `---`), and a list of all other files in the skill directory.

**Response:**
```json
{
  "name": "sandbox-execute",
  "description": "...",
  "compatibility": "...",
  "metadata": {...},
  "content": "# Sandbox Code Execution\n\n...",
  "files": [
    {
      "name": "execute.sh",
      "path": "/workspace/.agent/skills/sandbox-execute/scripts/execute.sh"
    },
    {
      "name": "execute_with_network.sh",
      "path": "/workspace/.agent/skills/sandbox-execute/scripts/execute_with_network.sh"
    }
  ]
}
```

`files` is a recursive listing of all files under the skill directory except `SKILL.md`.

### 3. Get Skill File

```
GET /api/v1/sandboxes/:id/skills/:name/files/*filepath
```

Returns the raw content of an attached file. The `filepath` is relative to the skill directory.

**Example:** `GET /api/v1/sandboxes/:id/skills/sandbox-execute/files/scripts/execute.sh`

**Response:** Raw file content, `Content-Type: text/plain`.

Path is validated to stay within `/workspace/.agent/skills/:name/` — no `..` traversal allowed.

## New Files

- `pkg/types/skill.go` — `Skill`, `SkillFile`, `SkillListResponse`, `SkillResponse` types
- `internal/api/handler/skill.go` — `ListSkills`, `GetSkill`, `GetSkillFile` handlers

## Modified Files

- `internal/api/router.go` — register three new routes under `v1`

## Implementation Notes

- Handlers reuse `h.manager` (existing `*sandbox.Manager`) — no new dependencies injected into `Handler`
- File reads go through `h.manager.DownloadFile` and `h.manager.ListFiles` (existing runtime methods)
- YAML frontmatter parsing uses `github.com/goccy/go-yaml` (already in `go.mod`)
- Frontmatter is delimited by `---` lines; body is everything after the second `---`
- All three endpoints return 404 if the sandbox does not exist
- `GetSkill` and `GetSkillFile` return 404 if the skill directory or file does not exist

## Error Handling

| Condition | Status |
|-----------|--------|
| Sandbox not found | 404 |
| Skills directory missing | 200 with empty `skills` list |
| Skill directory not found | 404 |
| `SKILL.md` missing | 404 |
| Attached file not found | 404 |
| Frontmatter parse error | 200, name from dir, other fields empty |
| Path traversal attempt | 400 |

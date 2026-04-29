package handler

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/logger"
	"github.com/goccy/go-yaml"

	"github.com/goairix/sandbox/internal/telemetry/trace"
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

	// Parse all frontmatter fields into a generic map so that unknown fields
	// (e.g. context, model, agent) are captured in Metadata automatically.
	var all map[string]interface{}
	if err := yaml.Unmarshal([]byte(frontmatter), &all); err != nil {
		return meta, body, fmt.Errorf("parse frontmatter: %w", err)
	}

	// Extract known top-level fields.
	knownKeys := map[string]bool{"name": true, "description": true, "compatibility": true, "metadata": true}
	if v, ok := all["name"].(string); ok {
		meta.Name = v
	}
	if v, ok := all["description"].(string); ok {
		meta.Description = v
	}
	if v, ok := all["compatibility"].(string); ok {
		meta.Compatibility = v
	}

	// Collect unknown top-level fields into Metadata.
	metadata := make(map[string]string)
	for k, v := range all {
		if knownKeys[k] {
			continue
		}
		metadata[k] = fmt.Sprintf("%v", v)
	}

	// Merge explicit "metadata" block entries (they take precedence).
	if m, ok := all["metadata"].(map[string]interface{}); ok {
		for k, v := range m {
			metadata[k] = fmt.Sprintf("%v", v)
		}
	}

	if len(metadata) > 0 {
		meta.Metadata = metadata
	}
	return meta, body, nil
}

func (h *Handler) listSkillFilesRecursive(ctx context.Context, id, skillRoot string) ([]types.SkillFile, error) {
	var out []types.SkillFile
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := h.manager.ListFiles(ctx, id, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir {
				if err := walk(e.Path); err != nil {
					return err
				}
				continue
			}
			rel := strings.TrimPrefix(e.Path, skillRoot+"/")
			if rel == "SKILL.md" {
				continue
			}
			out = append(out, types.SkillFile{
				Name: e.Name,
				Path: e.Path,
			})
		}
		return nil
	}
	if err := walk(skillRoot); err != nil {
		return nil, err
	}
	return out, nil
}

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

// GetSkill handles GET /api/v1/sandboxes/:id/skills/:name
func (h *Handler) GetSkill(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.skill.GetSkill")
	defer span.End()

	id := c.Param("id")
	name := c.Param("name")

	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid skill name"})
		return
	}

	if _, err := h.manager.Get(spanCtx, id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	skillMDPath := skillsBasePath + "/" + name + "/SKILL.md"
	tarReader, err := h.manager.DownloadFile(spanCtx, id, skillMDPath)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: "skill not found"})
		return
	}
	defer tarReader.Close()

	tr := tar.NewReader(tarReader)
	hdr, err := tr.Next()
	if err != nil {
		internalError(c, err)
		return
	}
	if hdr.Typeflag != tar.TypeReg {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: "skill not found"})
		return
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		internalError(c, err)
		return
	}

	meta, body, err := ParseFrontmatter(string(raw))
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid skill format: " + err.Error()})
		return
	}
	if meta.Name == "" {
		meta.Name = name
	}

	skillDir := skillsBasePath + "/" + name
	skillFiles, err := h.listSkillFilesRecursive(spanCtx, id, skillDir)
	if err != nil {
		logger.Warn(spanCtx, "failed to list skill files",
			logger.AddField("skill", name),
			logger.ErrorField(err))
		skillFiles = []types.SkillFile{}
	}

	c.JSON(http.StatusOK, types.SkillResponse{
		SkillMeta: meta,
		Path:      skillDir,
		Content:   body,
		Files:     skillFiles,
	})
}

// GetSkillFile handles GET /api/v1/sandboxes/:id/skills/:name/files/*filepath
func (h *Handler) GetSkillFile(c *gin.Context) {
	spanCtx, span := trace.Tracer().Start(trace.Gin(c), "api.skill.GetSkillFile")
	defer span.End()

	id := c.Param("id")
	name := c.Param("name")
	relPath := c.Param("filepath")

	if _, err := h.manager.Get(spanCtx, id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	skillRoot := skillsBasePath + "/" + name
	rel := strings.TrimPrefix(relPath, "/")
	if rel == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid path"})
		return
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid path"})
		return
	}
	fullPath := filepath.Join(skillRoot, cleaned)
	if !strings.HasPrefix(fullPath, skillRoot+"/") {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: "invalid path"})
		return
	}

	reader, err := h.manager.DownloadFile(spanCtx, id, fullPath)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: "file not found"})
		return
	}
	defer reader.Close()

	safeName := filepath.Base(fullPath)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", safeName))
	if _, err := io.Copy(c.Writer, reader); err != nil {
		_ = err
	}
}

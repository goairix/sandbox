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

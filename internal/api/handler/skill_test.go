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

func TestParseFrontmatter_UnknownFieldsInMetadata(t *testing.T) {
	input := "---\nname: code-analysis\ndescription: 深度代码分析技能\ncontext: fork\nmodel: deepseek-r1\nagent: code-agent\n---\n\n# Body\n"
	meta, body, err := handler.ParseFrontmatter(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Name != "code-analysis" {
		t.Errorf("name: got %q, want %q", meta.Name, "code-analysis")
	}
	if meta.Description != "深度代码分析技能" {
		t.Errorf("description: got %q", meta.Description)
	}
	// Unknown fields should be in Metadata.
	for _, kv := range []struct{ k, v string }{
		{"context", "fork"},
		{"model", "deepseek-r1"},
		{"agent", "code-agent"},
	} {
		if got := meta.Metadata[kv.k]; got != kv.v {
			t.Errorf("metadata[%q]: got %q, want %q", kv.k, got, kv.v)
		}
	}
	if !strings.Contains(body, "# Body") {
		t.Errorf("body missing content: %q", body)
	}
}

func TestParseFrontmatter_MixedExplicitAndUnknownMetadata(t *testing.T) {
	input := "---\nname: test\ndescription: test skill\ncontext: fork\nmetadata:\n  author: alice\n  version: \"1.0\"\n---\n\nbody\n"
	meta, _, err := handler.ParseFrontmatter(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Explicit metadata block entries should be present.
	if meta.Metadata["author"] != "alice" {
		t.Errorf("metadata[author]: got %q, want %q", meta.Metadata["author"], "alice")
	}
	if meta.Metadata["version"] != "1.0" {
		t.Errorf("metadata[version]: got %q, want %q", meta.Metadata["version"], "1.0")
	}
	// Unknown top-level field should also be present.
	if meta.Metadata["context"] != "fork" {
		t.Errorf("metadata[context]: got %q, want %q", meta.Metadata["context"], "fork")
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

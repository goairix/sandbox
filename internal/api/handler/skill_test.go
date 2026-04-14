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

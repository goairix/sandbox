package storage_test

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"github.com/goairix/sandbox/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildWorkspacePrefix(t *testing.T) {
	tests := []struct {
		name          string
		subPath       string
		workspacePath string
		want          string
	}{
		{
			name:          "empty sub path preserves workspace Unicode bytes",
			subPath:       "",
			workspacePath: "team/项目",
			want:          "team/项目/",
		},
		{
			name:          "sub path and workspace path",
			subPath:       "workspaces",
			workspacePath: "team/project",
			want:          "workspaces/team/project/",
		},
		{
			name:          "Unicode bytes are not normalized",
			subPath:       "团队/e\u0301",
			workspacePath: "项目/é",
			want:          "团队/e\u0301/项目/é/",
		},
		{
			name:          "spaces remain unchanged",
			subPath:       "team space",
			workspacePath: "project name",
			want:          "team space/project name/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := storage.BuildWorkspacePrefix(tt.subPath, tt.workspacePath)
			require.NoError(t, err)
			assert.Equal(t, []byte(tt.want), []byte(got))
			assert.True(t, strings.HasSuffix(got, "/"))
			assert.False(t, strings.HasSuffix(got, "//"))
			assert.False(t, strings.HasSuffix(strings.TrimSuffix(got, "/"), "/"))
		})
	}
}

func TestBuildWorkspacePrefixRejectsNonCanonicalWorkspacePath(t *testing.T) {
	invalid := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "leading slash", value: "/root"},
		{name: "trailing slash", value: "root/"},
		{name: "double slash", value: "a//b"},
		{name: "dot", value: "."},
		{name: "dot dot", value: ".."},
		{name: "dot segment", value: "a/./b"},
		{name: "dot dot segment", value: "a/../b"},
		{name: "reserved first segment", value: ".sandbox-system"},
		{name: "reserved first segment with child", value: ".sandbox-system/owner"},
		{name: "NUL", value: "a\x00b"},
		{name: "newline", value: "a\nb"},
		{name: "invalid UTF-8", value: string([]byte{0xff})},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := storage.BuildWorkspacePrefix("workspaces", tt.value)
			require.Error(t, err)
			assert.ErrorIs(t, err, storage.ErrInvalidWorkspacePrefix)
			assert.Contains(t, err.Error(), "workspace_path")
		})
	}
}

func TestBuildWorkspacePrefixRejectsNonCanonicalSubPath(t *testing.T) {
	invalid := []struct {
		name  string
		value string
	}{
		{name: "leading slash", value: "/root"},
		{name: "trailing slash", value: "root/"},
		{name: "double slash", value: "a//b"},
		{name: "dot", value: "."},
		{name: "dot dot", value: ".."},
		{name: "dot segment", value: "a/./b"},
		{name: "dot dot segment", value: "a/../b"},
		{name: "reserved first segment", value: ".sandbox-system"},
		{name: "reserved first segment with child", value: ".sandbox-system/owner"},
		{name: "NUL", value: "a\x00b"},
		{name: "newline", value: "a\nb"},
		{name: "invalid UTF-8", value: string([]byte{0xff})},
	}

	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := storage.BuildWorkspacePrefix(tt.value, "workspace")
			require.Error(t, err)
			assert.ErrorIs(t, err, storage.ErrInvalidWorkspacePrefix)
			assert.Contains(t, err.Error(), "sub_path")
		})
	}
}

func TestBuildWorkspacePrefixRejectsEveryControlRune(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.IsControl(r) {
			continue
		}
		_, err := storage.BuildWorkspacePrefix("workspaces", "a"+string(r)+"b")
		if !errors.Is(err, storage.ErrInvalidWorkspacePrefix) {
			t.Fatalf("control rune U+%04X: expected ErrInvalidWorkspacePrefix, got %v", r, err)
		}
	}
}

func TestBuildWorkspacePrefixErrorDoesNotLeakInput(t *testing.T) {
	t.Run("sub path", func(t *testing.T) {
		_, err := storage.BuildWorkspacePrefix("../private-sub-path", "do-not-leak-workspace-path")
		require.Error(t, err)
		assert.Equal(t, "sub_path: "+storage.ErrInvalidWorkspacePrefix.Error(), err.Error())
		assert.NotContains(t, err.Error(), "private-sub-path")
		assert.NotContains(t, err.Error(), "do-not-leak-workspace-path")
	})

	t.Run("workspace path", func(t *testing.T) {
		_, err := storage.BuildWorkspacePrefix("do-not-leak-sub-path", "../private-workspace-path")
		require.Error(t, err)
		assert.Equal(t, "workspace_path: "+storage.ErrInvalidWorkspacePrefix.Error(), err.Error())
		assert.NotContains(t, err.Error(), "do-not-leak-sub-path")
		assert.NotContains(t, err.Error(), "private-workspace-path")
	})
}

package sandbox

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsExcluded(t *testing.T) {
	exclude := []string{".agent", ".cache"}

	// Exact match
	assert.True(t, isExcluded(".agent", exclude))
	assert.True(t, isExcluded(".cache", exclude))

	// Prefix match (subdirectories / files)
	assert.True(t, isExcluded(".agent/IDENTITY.md", exclude))
	assert.True(t, isExcluded(".agent/skills/code.yaml", exclude))
	assert.True(t, isExcluded(".cache/tmp.dat", exclude))

	// Directory entries (trailing slash)
	assert.True(t, isExcluded(".agent/", exclude))
	assert.True(t, isExcluded(".agent/skills/", exclude))

	// Non-excluded paths
	assert.False(t, isExcluded("src/main.py", exclude))
	assert.False(t, isExcluded("data/input.csv", exclude))
	assert.False(t, isExcluded(".agentx/other", exclude))
	assert.False(t, isExcluded("my.agent/file", exclude))

	// Empty exclude list
	assert.False(t, isExcluded(".agent", nil))
	assert.False(t, isExcluded(".agent", []string{}))
}

func TestSyncFromContainer_ExcludeFiltering(t *testing.T) {
	// Test that excluded paths are filtered from manifest
	manifest := map[string]int64{
		"src/main.py":             100,
		".agent/IDENTITY.md":      200,
		".agent/skills/code.yaml": 200,
		".agent/":                 200,
		"data/input.csv":          100,
	}

	exclude := []string{".agent"}

	// Simulate changed set building with exclude
	changedSet := make(map[string]struct{})
	var cutoff int64 = 0
	for path, modtime := range manifest {
		if strings.HasSuffix(path, "/") {
			continue
		}
		if isExcluded(path, exclude) {
			continue
		}
		if cutoff == 0 || modtime > cutoff {
			changedSet[path] = struct{}{}
		}
	}

	assert.Contains(t, changedSet, "src/main.py")
	assert.Contains(t, changedSet, "data/input.csv")
	assert.NotContains(t, changedSet, ".agent/IDENTITY.md")
	assert.NotContains(t, changedSet, ".agent/skills/code.yaml")

	// Simulate deleted files building with exclude
	storageFiles := map[string]struct{}{
		"src/main.py":    {},
		"old_file.txt":   {},
		".agent/SOUL.md": {},
	}

	var deletedFiles []string
	for path := range storageFiles {
		if isExcluded(path, exclude) {
			continue
		}
		if _, exists := manifest[path]; !exists {
			deletedFiles = append(deletedFiles, path)
		}
	}

	assert.Contains(t, deletedFiles, "old_file.txt")
	assert.NotContains(t, deletedFiles, ".agent/SOUL.md")
}

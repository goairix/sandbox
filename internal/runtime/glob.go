package runtime

import "strings"

// GlobToFindArgs translates a glob pattern into find command arguments.
// It handles:
//   - {a,b} brace expansion → multiple -name conditions with -o
//   - ** recursive matching (uses no -maxdepth)
//   - * without ** at the start → -maxdepth 1 (current dir only)
//   - literal name patterns (including Unicode)
//
// Returns: (findArgs string, maxDepth1 bool)
// findArgs is the predicate portion of the find command (e.g. "-name '*.txt'").
// maxDepth1 is true if the pattern implies non-recursive matching.
func GlobToFindArgs(pattern string) (findArgs string, maxDepth1 bool) {
	recursive := strings.Contains(pattern, "**/") || strings.Contains(pattern, "**")
	cleaned := strings.TrimPrefix(pattern, "**/")

	if braceStart := strings.Index(cleaned, "{"); braceStart != -1 {
		braceEnd := strings.Index(cleaned, "}")
		if braceEnd > braceStart {
			prefix := cleaned[:braceStart]
			suffix := cleaned[braceEnd+1:]
			alternatives := strings.Split(cleaned[braceStart+1:braceEnd], ",")

			var parts []string
			for _, alt := range alternatives {
				name := prefix + alt + suffix
				parts = append(parts, "-name '"+escapeFindPattern(name)+"'")
			}
			findArgs = "\\( " + strings.Join(parts, " -o ") + " \\)"
			maxDepth1 = !recursive
			return
		}
	}

	findArgs = "-name '" + escapeFindPattern(cleaned) + "'"
	maxDepth1 = !recursive
	return
}

// escapeFindPattern escapes single quotes inside a find -name pattern value.
// The pattern itself (*, ?) must remain unescaped for find to interpret them.
func escapeFindPattern(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

package format

import (
	"path/filepath"
	"strings"
)

// IsExcluded reports whether path matches any exclude pattern in cfg.
// Patterns are matched against the path relative to baseDir, using '/' as separator.
// Supports '**' to match any number of path segments.
func IsExcluded(path string, baseDir string, cfg Config) bool {
	if len(cfg.Exclude) == 0 {
		return false
	}
	rel, ok := relPath(baseDir, path)
	if !ok {
		return false
	}
	for _, pattern := range cfg.Exclude {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if matchPattern(pattern, rel) {
			return true
		}
	}
	return false
}

func matchPattern(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)
	patSegs := splitSegments(pattern)
	pathSegs := splitSegments(path)
	return matchSegments(patSegs, pathSegs)
}

func splitSegments(s string) []string {
	parts := strings.Split(s, "/")
	out := parts[:0]
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		out = append(out, p)
	}
	return out
}

func matchSegments(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if matchSegments(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], path[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], path[1:])
}

func relPath(baseDir, path string) (string, bool) {
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(baseAbs, pathAbs)
	if err != nil {
		return "", false
	}
	if rel == "." {
		return ".", true
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

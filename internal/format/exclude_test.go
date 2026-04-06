package format_test

import (
	"path/filepath"
	"testing"

	"github.com/vearutop/gocan/internal/format"
)

func TestExcludeMatcher(t *testing.T) {
	base := filepath.Join(string(filepath.Separator), "repo")
	cfg := format.Config{
		Exclude: []string{
			"**/*_generated.go",
			"internal/legacy/**",
			"cmd/**/testdata/*",
		},
	}

	assertExcluded(t, base, filepath.Join(base, "internal/legacy/old/file.go"), cfg)
	assertExcluded(t, base, filepath.Join(base, "pkg/foo/bar_generated.go"), cfg)
	assertExcluded(t, base, filepath.Join(base, "cmd/tool/testdata/input.go"), cfg)
	assertNotExcluded(t, base, filepath.Join(base, "cmd/tool/testdata/deep/input.go"), cfg)
	assertNotExcluded(t, base, filepath.Join(base, "internal/new/file.go"), cfg)
}

func assertExcluded(t *testing.T, base, path string, cfg format.Config) {
	t.Helper()
	if !format.IsExcluded(path, base, cfg) {
		t.Fatalf("expected excluded: %s", path)
	}
}

func assertNotExcluded(t *testing.T, base, path string, cfg format.Config) {
	t.Helper()
	if format.IsExcluded(path, base, cfg) {
		t.Fatalf("expected not excluded: %s", path)
	}
}

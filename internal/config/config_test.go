package config

import "testing"

func TestCollapsed(t *testing.T) {
	r := RepoConfig{CollapsedGlobs: []string{"**/*.pb.go", "pnpm-lock.yaml", "vendor/**"}}
	for path, want := range map[string]bool{
		"api/v1/foo.pb.go":     true,
		"foo.pb.go":            true,
		"pnpm-lock.yaml":       true,
		"web/pnpm-lock.yaml":   false,
		"vendor/a/b.go":        true,
		"internal/vendor/a.go": false,
		"main.go":              false,
	} {
		if got := r.Collapsed(path); got != want {
			t.Errorf("Collapsed(%q) = %v, want %v", path, got, want)
		}
	}
}

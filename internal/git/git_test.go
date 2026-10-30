package git

import "testing"

func TestSlug(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:owner/name.git\n":        "owner/name",
		"https://github.com/owner/name.git":      "owner/name",
		"https://github.com/owner/name":          "owner/name",
		"ssh://git@github.com:22/owner/name.git": "owner/name",
	} {
		if got := slug(url); got != want {
			t.Errorf("slug(%q) = %q, want %q", url, got, want)
		}
	}
}

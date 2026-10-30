// Package config loads the global di config.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Repos []RepoConfig `yaml:"repos"`
}

type RepoConfig struct {
	Repo           string   `yaml:"repo"`
	Base           string   `yaml:"base,omitempty"`
	CollapsedGlobs []string `yaml:"collapsed,omitempty"` // Collapsed is taken by the method
}

// path is $XDG_CONFIG_HOME/di/config.yaml (default ~/.config).
func path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "di", "config.yaml"), nil
}

// Load reads the config file. A missing file yields an empty config.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, yaml.Unmarshal(data, &c)
}

func (c *Config) Repo(slug string) RepoConfig {
	for _, r := range c.Repos {
		if slug != "" && r.Repo == slug {
			return r
		}
	}
	return RepoConfig{}
}

// SetCollapsed replaces slug's collapsed patterns and writes the config file.
func (c *Config) SetCollapsed(slug string, patterns []string) error {
	for _, p := range patterns {
		if !doublestar.ValidatePattern(p) {
			return fmt.Errorf("invalid pattern %q", p)
		}
	}
	if i := slices.IndexFunc(c.Repos, func(r RepoConfig) bool { return r.Repo == slug }); i >= 0 {
		c.Repos[i].CollapsedGlobs = patterns
	} else {
		c.Repos = append(c.Repos, RepoConfig{Repo: slug, CollapsedGlobs: patterns})
	}
	p, err := path()
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Collapsed reports whether path (relative to the repo root) matches any collapsed pattern.
func (r RepoConfig) Collapsed(path string) bool {
	return slices.ContainsFunc(r.CollapsedGlobs, func(p string) bool { return doublestar.MatchUnvalidated(p, path) })
}

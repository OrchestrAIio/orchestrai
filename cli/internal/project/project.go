// Package project handles the committable repo binding .orchestrai/config.json.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir is the per-repo directory holding config.json and manifest.json.
const Dir = ".orchestrai"

// Config is .orchestrai/config.json. It never contains secrets.
type Config struct {
	ProjectID string   `json:"project_id"`
	APIURL    string   `json:"api_url,omitempty"`
	AppURL    string   `json:"app_url,omitempty"`
	OrgID     string   `json:"org_id,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// ErrNotInitialized means no .orchestrai/config.json was found.
var ErrNotInitialized = errors.New("this repository is not bound to an OrchestrAI team space; run `orchestrai init`")

// ConfigPath returns root/.orchestrai/config.json.
func ConfigPath(root string) string { return filepath.Join(root, Dir, "config.json") }

// FindRoot walks up from start looking first for .orchestrai/config.json,
// then for a .git entry. Without either it returns start.
func FindRoot(start string) (root string, bound bool, err error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false, err
	}
	for d := abs; ; {
		if _, err := os.Stat(ConfigPath(d)); err == nil {
			return d, true, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	for d := abs; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, false, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return abs, false, nil
}

// Load reads root/.orchestrai/config.json.
func Load(root string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath(root), err)
	}
	if c.ProjectID == "" {
		return nil, fmt.Errorf("%s has no project_id; run `orchestrai init`", ConfigPath(root))
	}
	return &c, nil
}

// Save writes root/.orchestrai/config.json.
func Save(root string, c *Config) error {
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(root), append(data, '\n'), 0o644)
}

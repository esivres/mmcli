// Package config persists mmcli connection contexts in a JSON file under
// the user's XDG config dir. Secrets (password, cached token) are NOT stored
// here — they live in the keyring (see internal/secrets).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNoContext means no context is selected / configured yet.
var ErrNoContext = errors.New("no context configured; run `mmcli login` first")

// Context holds non-secret connection parameters for one Mattermost server.
type Context struct {
	URL         string `json:"url"`
	LoginID     string `json:"login_id"`
	DefaultTeam string `json:"default_team,omitempty"`
}

// Config is the on-disk document. The file path is kept separately and is not
// serialized.
type Config struct {
	CurrentName string             `json:"current"`
	Contexts    map[string]Context `json:"contexts"`

	path string
}

// DefaultPath returns the config file location, honoring XDG_CONFIG_HOME.
func DefaultPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "mmcli", "config.json"), nil
}

// Load reads the config at path. A missing file yields an empty, ready-to-use
// Config rather than an error.
func Load(path string) (*Config, error) {
	c := &Config{Contexts: map[string]Context{}, path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	c.path = path
	return c, nil
}

// Save writes the config back to disk with 0600 permissions.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Current returns the active context and its name.
func (c *Config) Current() (string, Context, error) {
	if c.CurrentName == "" {
		return "", Context{}, ErrNoContext
	}
	ctx, ok := c.Contexts[c.CurrentName]
	if !ok {
		return "", Context{}, fmt.Errorf("current context %q not found", c.CurrentName)
	}
	return c.CurrentName, ctx, nil
}

// Get returns a named context.
func (c *Config) Get(name string) (Context, bool) {
	ctx, ok := c.Contexts[name]
	return ctx, ok
}

// Set stores/replaces a context and makes it current.
func (c *Config) Set(name string, ctx Context) {
	c.Contexts[name] = ctx
	c.CurrentName = name
}

// Use switches the active context.
func (c *Config) Use(name string) error {
	if _, ok := c.Contexts[name]; !ok {
		return fmt.Errorf("context %q not found", name)
	}
	c.CurrentName = name
	return nil
}

// Delete removes a context; clears Current if it pointed there.
func (c *Config) Delete(name string) {
	delete(c.Contexts, name)
	if c.CurrentName == name {
		c.CurrentName = ""
	}
}

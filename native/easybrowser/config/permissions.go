package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// CDPPermissionConfig controls which CDP capabilities are available.
// All permissions default to true (maximum capability). Users can toggle
// individual permissions OFF in the extension popup for security hardening.
type CDPPermissionConfig struct {
	AllowRawCDP      bool `json:"allow_raw_cdp"`       // default: true — browser_cdp_call tool
	AllowEvalJS      bool `json:"allow_eval_js"`       // default: true — browser_evaluate_js tool
	AllowFetchDomain bool `json:"allow_fetch_domain"` // default: true — Fetch.enable/disable CDP methods
}

// DefaultPermissions returns the default permission config (all ON).
func DefaultPermissions() CDPPermissionConfig {
	return CDPPermissionConfig{
		AllowRawCDP:      true,
		AllowEvalJS:      true,
		AllowFetchDomain: true,
	}
}

// PermissionStore manages CDP permissions with in-memory cache and file persistence.
type PermissionStore struct {
	mu     sync.RWMutex
	config CDPPermissionConfig
	path   string
}

// NewPermissionStore creates a new PermissionStore, loading from file if it exists
// or using defaults.
func NewPermissionStore() (*PermissionStore, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("get config dir: %w", err)
	}
	dir := filepath.Join(configDir, "browser-mcp")
	path := filepath.Join(dir, "cdp-permissions.json")

	store := &PermissionStore{
		config: DefaultPermissions(),
		path:   path,
	}

	// Try to load existing config
	if data, err := os.ReadFile(path); err == nil {
		var loaded CDPPermissionConfig
		if err := json.Unmarshal(data, &loaded); err == nil {
			store.config = loaded
		}
	}

	return store, nil
}

// Get returns a copy of the current permission config.
func (s *PermissionStore) Get() CDPPermissionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// Set updates a single permission key and persists to file.
// Valid keys: allow_raw_cdp, allow_eval_js, allow_fetch_domain
func (s *PermissionStore) Set(key string, value bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch key {
	case "allow_raw_cdp":
		s.config.AllowRawCDP = value
	case "allow_eval_js":
		s.config.AllowEvalJS = value
	case "allow_fetch_domain":
		s.config.AllowFetchDomain = value
	default:
		return fmt.Errorf("unknown permission key: %s", key)
	}

	return s.persist()
}

// persist writes the current config to file.
func (s *PermissionStore) persist() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(s.path, data, 0644); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}

	return nil
}

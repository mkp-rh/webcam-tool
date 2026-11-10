package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	fileName = "webcam.conf"
	version  = 1
)

type Config struct {
	mu sync.RWMutex

	path string

	Version    int                          `json:"version"`
	Last       string                       `json:"last_camera"`
	Cameras    map[string]map[string]int64  `json:"cameras"`
}

func Load() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return &Config{path: "", Version: version, Cameras: map[string]map[string]int64{}}, err
	}

	cfg := &Config{
		path:    p,
		Version: version,
		Cameras: map[string]map[string]int64{},
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if len(b) == 0 {
		return cfg, nil
	}

	if err := json.Unmarshal(b, cfg); err != nil {
		// keep defaults, but surface error
		return cfg, err
	}
	if cfg.Cameras == nil {
		cfg.Cameras = map[string]map[string]int64{}
	}
	if cfg.Version == 0 {
		cfg.Version = version
	}
	cfg.path = p
	return cfg, nil
}

func (c *Config) Save() error {
	c.mu.RLock()
	b, err := json.MarshalIndent(c, "", "  ")
	path := c.path
	c.mu.RUnlock()
	if err != nil {
		return err
	}
	if path == "" {
		p, err := configPath()
		if err != nil {
			return err
		}
		path = p
		c.mu.Lock()
		c.path = p
		c.mu.Unlock()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) LastCamera() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Last
}

func (c *Config) SetLastCamera(device string) {
	c.mu.Lock()
	c.Last = device
	c.mu.Unlock()
}

func (c *Config) CameraControls(device string) map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := c.Cameras[device]
	if m == nil {
		return map[string]int64{}
	}
	cp := make(map[string]int64, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func (c *Config) SetCameraControls(device string, values map[string]int64) {
	c.mu.Lock()
	if c.Cameras == nil {
		c.Cameras = map[string]map[string]int64{}
	}
	cp := make(map[string]int64, len(values))
	for k, v := range values {
		cp[k] = v
	}
	c.Cameras[device] = cp
	c.mu.Unlock()
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", fileName), nil
}


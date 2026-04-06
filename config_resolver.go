// Package main provides the gocan CLI.
package main

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/vearutop/gocan/internal/format"
)

const configFileName = ".gocan.json"

func newConfigResolver() *configResolver {
	return &configResolver{
		cache: make(map[string]resolvedConfig),
	}
}

func resolveConfigForDir(dir string) (format.Config, string, error) {
	orig := dir
	for {
		cfgPath := filepath.Join(dir, configFileName)
		if info, err := os.Stat(cfgPath); err == nil && !info.IsDir() {
			cfg, err := format.LoadConfig(cfgPath)
			if err != nil {
				return format.Config{}, "", err
			}
			if err := format.ValidateConfig(cfg); err != nil {
				return format.Config{}, "", err
			}
			return cfg, dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	cfg := format.DefaultConfig()
	if err := format.ValidateConfig(cfg); err != nil {
		return format.Config{}, "", err
	}
	return cfg, orig, nil
}

type configResolver struct {
	mu    sync.Mutex
	cache map[string]resolvedConfig // dir -> resolved config
}

func (r *configResolver) Resolve(filePath string) (format.Config, string, error) {
	dir := filepath.Dir(filePath)

	r.mu.Lock()
	if cached, ok := r.cache[dir]; ok {
		r.mu.Unlock()
		return cached.cfg, cached.baseDir, nil
	}
	r.mu.Unlock()

	cfg, baseDir, err := resolveConfigForDir(dir)
	if err != nil {
		return format.Config{}, "", err
	}

	r.mu.Lock()
	r.cache[dir] = resolvedConfig{cfg: cfg, baseDir: baseDir}
	r.mu.Unlock()
	return cfg, baseDir, nil
}

type resolvedConfig struct {
	cfg     format.Config
	baseDir string
}

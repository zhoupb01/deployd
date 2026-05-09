package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultListen   = "127.0.0.1:8080"
	defaultStateDir = "/var/lib/deployd"
	defaultTimeout  = 5 * time.Minute
	maxTimeout      = 30 * time.Minute
)

var serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

type Config struct {
	Listen   string                    `yaml:"listen"`
	StateDir string                    `yaml:"state_dir"`
	Services map[string]*ServiceConfig `yaml:"services"`
}

type ServiceConfig struct {
	Name           string        `yaml:"-"`
	SecretEnv      string        `yaml:"secret_env"`
	Workdir        string        `yaml:"workdir"`
	ComposeService string        `yaml:"compose_service"`
	Timeout        time.Duration `yaml:"timeout"`

	Secret []byte `yaml:"-"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir
	}
	if !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("state_dir must be absolute path: %q", c.StateDir)
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one service must be configured")
	}
	for name, svc := range c.Services {
		if !serviceNameRe.MatchString(name) {
			return fmt.Errorf("service name %q must match %s", name, serviceNameRe)
		}
		svc.Name = name
		if err := svc.normalize(); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
	}
	return nil
}

func (s *ServiceConfig) normalize() error {
	if s.SecretEnv == "" {
		return fmt.Errorf("secret_env is required")
	}
	secret := os.Getenv(s.SecretEnv)
	if secret == "" {
		return fmt.Errorf("env var %s is empty", s.SecretEnv)
	}
	s.Secret = []byte(secret)

	if s.Workdir == "" {
		return fmt.Errorf("workdir is required")
	}
	if !filepath.IsAbs(s.Workdir) {
		return fmt.Errorf("workdir must be absolute path: %q", s.Workdir)
	}

	if s.Timeout <= 0 {
		s.Timeout = defaultTimeout
	}
	if s.Timeout > maxTimeout {
		return fmt.Errorf("timeout %s exceeds max %s", s.Timeout, maxTimeout)
	}
	return nil
}

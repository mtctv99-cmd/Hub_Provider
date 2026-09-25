package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Host                 string `yaml:"host"`
	Port                 int    `yaml:"port"`
	DBPath               string `yaml:"db_path"`
	APIKey               string `yaml:"api_key"`
	MasterKey            string `yaml:"master_key"`
	ShutdownGraceSec     int    `yaml:"shutdown_grace_sec"`
	MaxRequestBodyBytes  int64  `yaml:"max_request_body_bytes"`
	LeaseReclaimInterval int    `yaml:"lease_reclaim_interval_sec"`
}
type Model struct {
	Provider     string   `yaml:"provider"`
	Capabilities []string `yaml:"capabilities"`
	FailoverTo   []string `yaml:"failover_to"`
	TimeoutSec   int      `yaml:"timeout_sec"`
	Guest        bool     `yaml:"guest"`
}
type Config struct {
	Server Server           `yaml:"server"`
	Models map[string]Model `yaml:"models"`
}

func Load(dir string) (Config, error) {
	cfg := Config{
		Server: Server{
			Host:                 "127.0.0.1",
			Port:                 8080,
			DBPath:               "data/orchestrator.db",
			ShutdownGraceSec:     30,
			MaxRequestBodyBytes:  10 * 1024 * 1024,
			LeaseReclaimInterval: 15,
		},
		Models: map[string]Model{},
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	}
	modelsPath := filepath.Join(dir, "models.yaml")
	if data, err := os.ReadFile(modelsPath); err == nil {
		var wrap struct {
			Models map[string]Model `yaml:"models"`
		}
		if err := yaml.Unmarshal(data, &wrap); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", modelsPath, err)
		}
		for k, v := range wrap.Models {
			cfg.Models[k] = v
		}
	}
		if h := os.Getenv("SERVER_HOST"); h != "" {
			cfg.Server.Host = h
		} else if h := os.Getenv("HOST"); h != "" {
			cfg.Server.Host = h
		}
		if p := os.Getenv("SERVER_PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				cfg.Server.Port = n
			}
		} else if p := os.Getenv("PORT"); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				cfg.Server.Port = n
			}
		}
	if path := os.Getenv("DB_PATH"); path != "" {
		cfg.Server.DBPath = path
	}
	if key := os.Getenv("API_KEY"); key != "" {
		cfg.Server.APIKey = key
	}
	if maxBody := os.Getenv("MAX_REQUEST_BODY_BYTES"); maxBody != "" {
		if n, err := strconv.ParseInt(maxBody, 10, 64); err == nil && n > 0 {
			cfg.Server.MaxRequestBodyBytes = n
		}
	}
	if reclaim := os.Getenv("LEASE_RECLAIM_INTERVAL_SEC"); reclaim != "" {
		if n, err := strconv.Atoi(reclaim); err == nil && n > 0 {
			cfg.Server.LeaseReclaimInterval = n
		}
	}
	return cfg, nil
}

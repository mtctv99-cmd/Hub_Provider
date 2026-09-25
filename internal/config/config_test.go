package config

import "testing"

func TestConfigDefaults(t *testing.T) {
	cfg, err := Load("non-existent-dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("host=%q, want 127.0.0.1", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port=%d, want 8080", cfg.Server.Port)
	}
	if cfg.Server.MaxRequestBodyBytes != 10*1024*1024 {
		t.Errorf("maxRequestBodyBytes=%d, want 10MB", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.LeaseReclaimInterval != 15 {
		t.Errorf("leaseReclaimInterval=%d, want 15", cfg.Server.LeaseReclaimInterval)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	t.Setenv("SERVER_PORT", "19090")
	t.Setenv("API_KEY", "test-secret-key")
	t.Setenv("MAX_REQUEST_BODY_BYTES", "2097152") // 2MB
	t.Setenv("LEASE_RECLAIM_INTERVAL_SEC", "30")

	cfg, err := Load("non-existent-dir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 19090 {
		t.Errorf("port=%d, want 19090", cfg.Server.Port)
	}
	if cfg.Server.APIKey != "test-secret-key" {
		t.Errorf("apiKey=%q, want test-secret-key", cfg.Server.APIKey)
	}
	if cfg.Server.MaxRequestBodyBytes != 2097152 {
		t.Errorf("maxRequestBodyBytes=%d, want 2097152", cfg.Server.MaxRequestBodyBytes)
	}
	if cfg.Server.LeaseReclaimInterval != 30 {
		t.Errorf("leaseReclaimInterval=%d, want 30", cfg.Server.LeaseReclaimInterval)
	}
}

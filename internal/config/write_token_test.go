package config

import (
	"strings"
	"testing"
)

// The read/write split is opt-in: an unset (or empty) MCP_WRITE_BEARER_TOKEN
// must leave Load and the banner exactly as they were, so an existing install
// keeps the single-token surface on deploy.
func TestLoadWriteTokenUnsetIsLegacy(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}
	if cfg.MCPWriteBearerToken != "" {
		t.Errorf("MCPWriteBearerToken = %q, want empty", cfg.MCPWriteBearerToken)
	}
	if got := cfg.TokenMode(); got != "single" {
		t.Errorf("TokenMode() = %q, want single", got)
	}
	if s := cfg.String(); strings.Contains(s, "MCPWriteBearerToken") {
		t.Errorf("String() names the write token when unset: %q", s)
	}
}

// An empty value is what `MCP_WRITE_BEARER_TOKEN=` in an EnvironmentFile
// produces; systemd sets the variable to "", and that must read as unset
// rather than as a (too short) secret.
func TestLoadWriteTokenEmptyIsUnset(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)
	t.Setenv(EnvWriteBearerToken, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}
	if got := cfg.TokenMode(); got != "single" {
		t.Errorf("TokenMode() = %q, want single", got)
	}
}

func TestLoadWriteTokenSet(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)
	const write = "write-token-0123456789abcdef"
	t.Setenv(EnvWriteBearerToken, write)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}
	if cfg.MCPWriteBearerToken != write {
		t.Errorf("MCPWriteBearerToken = %q, want %q", cfg.MCPWriteBearerToken, write)
	}
	if cfg.MCPBearerToken != "test-token-ABCDEFGH" {
		t.Errorf("MCPBearerToken = %q; the read token must be untouched by the split", cfg.MCPBearerToken)
	}
	if got := cfg.TokenMode(); got != "split" {
		t.Errorf("TokenMode() = %q, want split", got)
	}
}

func TestLoadWriteTokenTooShort(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)
	t.Setenv(EnvWriteBearerToken, "short")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with a short %s: expected error, got %v", EnvWriteBearerToken, cfg)
	}
	if !strings.Contains(err.Error(), EnvWriteBearerToken) {
		t.Errorf("error %q does not name %s", err, EnvWriteBearerToken)
	}
	if !strings.Contains(err.Error(), "16") {
		t.Errorf("error %q does not state the minimum length", err)
	}
}

// Equal values would deploy a "split" where the read token still writes —
// the exact opposite of the point — so startup refuses instead of quietly
// degrading to one token.
func TestLoadWriteTokenMustDifferFromReadToken(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)
	t.Setenv(EnvWriteBearerToken, "test-token-ABCDEFGH") // same as setRequired's read token

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with equal tokens: expected error, got %v", cfg)
	}
	if !strings.Contains(err.Error(), EnvWriteBearerToken) {
		t.Errorf("error %q does not name %s", err, EnvWriteBearerToken)
	}
	if !strings.Contains(err.Error(), "differ") {
		t.Errorf("error %q does not say the tokens must differ", err)
	}
}

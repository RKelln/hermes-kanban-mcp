package config

import (
	"os"
	"strings"
	"testing"
)

// setRequired sets the three environment variables Load requires,
// using distinctive values that tests can assert on.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("KANBAN_USERNAME", "test-user")
	t.Setenv("KANBAN_PASSWORD", "test-password-42")
	t.Setenv("MCP_BEARER_TOKEN", "test-token-ABCDEFGH")
}

// unsetEnv removes the named variables for the duration of the test,
// restoring their prior state on cleanup. Tests in this package run
// sequentially, so mutating the process environment is safe here.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	saved := make(map[string]string, len(keys))
	wasSet := make(map[string]bool, len(keys))
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		saved[k], wasSet[k] = v, ok
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range keys {
			if wasSet[k] {
				os.Setenv(k, saved[k])
			} else {
				os.Unsetenv(k)
			}
		}
	})
}

var optionalVars = []string{
	"BIND_ADDRS",
	"KANBAN_BASE_URL",
	"KANBAN_DEFAULT_BOARD",
	"HERMES_BIN",
	"MCP_COMPLETE_MODE",
	"MCP_ALLOW_SKIP_CLAIM",
	"MCP_CLAIM_WORKER",
	"MCP_COMMENT_AUTHOR",
	"MCP_RATE_LIMIT",
	"LOG_LEVEL",
}

func TestLoadAppliesDefaults(t *testing.T) {
	setRequired(t)
	unsetEnv(t, optionalVars...)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with required vars set: unexpected error: %v", err)
	}

	want := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"BindAddrs", cfg.BindAddrs, "127.0.0.1:9130"},
		{"KanbanBaseURL", cfg.KanbanBaseURL, "http://127.0.0.1:9119/api/plugins/kanban/"},
		{"KanbanDefaultBoard", cfg.KanbanDefaultBoard, "hermes-agent"},
		{"HermesBin", cfg.HermesBin, "hermes"},
		{"MCPCompleteMode", cfg.MCPCompleteMode, "review"},
		{"MCPAllowSkipClaim", cfg.MCPAllowSkipClaim, false},
		{"MCPClaimWorker", cfg.MCPClaimWorker, "opencode-remote"},
		{"MCPCommentAuthor", cfg.MCPCommentAuthor, "opencode-remote"},
		{"MCPRateLimit", cfg.MCPRateLimit, 60},
		{"LogLevel", cfg.LogLevel, "info"},
	}
	for _, w := range want {
		if w.got != w.want {
			t.Errorf("%s = %v, want %v", w.name, w.got, w.want)
		}
	}
}

func TestLoadAppliesExplicitOverrides(t *testing.T) {
	setRequired(t)
	t.Setenv("BIND_ADDRS", "0.0.0.0:9130,127.0.0.1:9131")
	t.Setenv("KANBAN_BASE_URL", "http://kanban.internal:9999/api/plugins/kanban/")
	t.Setenv("KANBAN_USERNAME", "alice")
	t.Setenv("KANBAN_PASSWORD", "override-pw-123456")
	t.Setenv("MCP_BEARER_TOKEN", "override-token-9876543210")
	t.Setenv("KANBAN_DEFAULT_BOARD", "my-board")
	t.Setenv("HERMES_BIN", "/usr/local/bin/hermes")
	t.Setenv("MCP_COMPLETE_MODE", "done")
	t.Setenv("MCP_ALLOW_SKIP_CLAIM", "1")
	t.Setenv("MCP_CLAIM_WORKER", "worker-x")
	t.Setenv("MCP_COMMENT_AUTHOR", "author-x")
	t.Setenv("MCP_RATE_LIMIT", "150")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with full env: unexpected error: %v", err)
	}

	if cfg.BindAddrs != "0.0.0.0:9130,127.0.0.1:9131" {
		t.Errorf("BindAddrs = %q, want override", cfg.BindAddrs)
	}
	if cfg.KanbanBaseURL != "http://kanban.internal:9999/api/plugins/kanban/" {
		t.Errorf("KanbanBaseURL = %q, want override", cfg.KanbanBaseURL)
	}
	if cfg.KanbanUsername != "alice" {
		t.Errorf("KanbanUsername = %q, want override", cfg.KanbanUsername)
	}
	if cfg.KanbanPassword != "override-pw-123456" {
		t.Errorf("KanbanPassword = %q, want override", cfg.KanbanPassword)
	}
	if cfg.MCPBearerToken != "override-token-9876543210" {
		t.Errorf("MCPBearerToken = %q, want override", cfg.MCPBearerToken)
	}
	if cfg.KanbanDefaultBoard != "my-board" {
		t.Errorf("KanbanDefaultBoard = %q, want override", cfg.KanbanDefaultBoard)
	}
	if cfg.HermesBin != "/usr/local/bin/hermes" {
		t.Errorf("HermesBin = %q, want override", cfg.HermesBin)
	}
	if cfg.MCPCompleteMode != "done" {
		t.Errorf("MCPCompleteMode = %q, want override", cfg.MCPCompleteMode)
	}
	if !cfg.MCPAllowSkipClaim {
		t.Error("MCPAllowSkipClaim = false, want true")
	}
	if cfg.MCPClaimWorker != "worker-x" {
		t.Errorf("MCPClaimWorker = %q, want override", cfg.MCPClaimWorker)
	}
	if cfg.MCPCommentAuthor != "author-x" {
		t.Errorf("MCPCommentAuthor = %q, want override", cfg.MCPCommentAuthor)
	}
	if cfg.MCPRateLimit != 150 {
		t.Errorf("MCPRateLimit = %d, want 150", cfg.MCPRateLimit)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want override", cfg.LogLevel)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []string{"KANBAN_USERNAME", "KANBAN_PASSWORD", "MCP_BEARER_TOKEN"}
	for _, missing := range cases {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			unsetEnv(t, missing)

			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s unset: expected error, got %v", missing, cfg)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q does not name %s", err, missing)
			}
		})
	}
}

func TestLoadEmptyRequired(t *testing.T) {
	cases := []string{"KANBAN_USERNAME", "KANBAN_PASSWORD", "MCP_BEARER_TOKEN"}
	for _, empty := range cases {
		t.Run(empty, func(t *testing.T) {
			setRequired(t)
			t.Setenv(empty, "")

			cfg, err := Load()
			if err == nil {
				t.Fatalf("Load() with %s set empty: expected error, got %v", empty, cfg)
			}
			if !strings.Contains(err.Error(), empty) {
				t.Errorf("error %q does not name %s", err, empty)
			}
		})
	}
}

func TestLoadBearerTokenTooShort(t *testing.T) {
	setRequired(t)
	t.Setenv("MCP_BEARER_TOKEN", "short")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with 5-char token: expected error, got %v", cfg)
	}
	if !strings.Contains(err.Error(), "MCP_BEARER_TOKEN") {
		t.Errorf("error %q does not name MCP_BEARER_TOKEN", err)
	}
}

func TestLoadBearerTokenMinLength(t *testing.T) {
	setRequired(t)
	t.Setenv("MCP_BEARER_TOKEN", "0123456789abcdef") // exactly 16 chars

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with 16-char token: unexpected error: %v", err)
	}
	if cfg.MCPBearerToken != "0123456789abcdef" {
		t.Errorf("MCPBearerToken = %q, want 16-char value", cfg.MCPBearerToken)
	}
}

func TestLoadInvalidBool(t *testing.T) {
	setRequired(t)
	t.Setenv("MCP_ALLOW_SKIP_CLAIM", "banana")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with non-bool MCP_ALLOW_SKIP_CLAIM: expected error, got %v", cfg)
	}
	if !strings.Contains(err.Error(), "MCP_ALLOW_SKIP_CLAIM") {
		t.Errorf("error %q does not name MCP_ALLOW_SKIP_CLAIM", err)
	}
}

func TestLoadInvalidInt(t *testing.T) {
	setRequired(t)
	t.Setenv("MCP_RATE_LIMIT", "many")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with non-int MCP_RATE_LIMIT: expected error, got %v", cfg)
	}
	if !strings.Contains(err.Error(), "MCP_RATE_LIMIT") {
		t.Errorf("error %q does not name MCP_RATE_LIMIT", err)
	}
}

func TestLoadInvalidCompleteMode(t *testing.T) {
	setRequired(t)
	t.Setenv("MCP_COMPLETE_MODE", "auto")

	cfg, err := Load()
	if err == nil {
		t.Fatalf("Load() with MCP_COMPLETE_MODE=auto: expected error, got %v", cfg)
	}
	if !strings.Contains(err.Error(), "MCP_COMPLETE_MODE") {
		t.Errorf("error %q does not name MCP_COMPLETE_MODE", err)
	}
}

func TestStringRedactsSecrets(t *testing.T) {
	const (
		password = "hunter2-secret-value"
		token    = "tok-1234567890abcd"
	)
	t.Setenv("KANBAN_USERNAME", "test-user")
	t.Setenv("KANBAN_PASSWORD", password)
	t.Setenv("MCP_BEARER_TOKEN", token)
	unsetEnv(t, optionalVars...)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}

	s := cfg.String()
	if strings.Contains(s, password) {
		t.Errorf("String() leaks KANBAN_PASSWORD: %q", s)
	}
	if strings.Contains(s, token) {
		t.Errorf("String() leaks MCP_BEARER_TOKEN: %q", s)
	}
	if !strings.Contains(s, "KanbanPassword=***") {
		t.Errorf("String() does not mask KanbanPassword: %q", s)
	}
	if !strings.Contains(s, "MCPBearerToken=***") {
		t.Errorf("String() does not mask MCPBearerToken: %q", s)
	}
	// Every non-secret field must be present.
	for _, want := range []string{
		"BindAddrs=", "KanbanBaseURL=", "KanbanUsername=test-user",
		"KanbanDefaultBoard=", "HermesBin=", "MCPCompleteMode=",
		"MCPAllowSkipClaim=", "MCPClaimWorker=", "MCPCommentAuthor=",
		"MCPRateLimit=", "LogLevel=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q: %q", want, s)
		}
	}
}

// Package config loads and validates kanban-mcp server configuration
// from environment variables. Standard library only; no third-party
// dependencies.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds every runtime setting for the kanban-mcp server. Each
// field maps to a single environment variable; see Load for the full
// mapping, defaults, and validation rules.
type Config struct {
	// BindAddrs is a comma-separated list of host:port pairs the MCP
	// server should listen on. Env: BIND_ADDRS.
	BindAddrs string
	// KanbanBaseURL is the base URL of the kanban REST API. Env: KANBAN_BASE_URL.
	KanbanBaseURL string
	// KanbanUsername authenticates against the kanban API. Required. Env: KANBAN_USERNAME.
	KanbanUsername string
	// KanbanPassword authenticates against the kanban API. Required. Env: KANBAN_PASSWORD.
	KanbanPassword string
	// MCPBearerToken guards MCP tool calls. Required, min 16 chars. Env: MCP_BEARER_TOKEN.
	MCPBearerToken string
	// KanbanDefaultBoard is the board slug surfaced as informational by
	// board_list. Board-taking tools reject an omitted board, so this is
	// no longer used as a fallback. Env: KANBAN_DEFAULT_BOARD.
	KanbanDefaultBoard string
	// HermesBin is the hermes CLI binary invoked for hermes-mediated operations. Env: HERMES_BIN.
	HermesBin string
	// MCPCompleteMode controls how ticket completion is finalized: "review"
	// (block for human review) or "done" (complete directly). Env: MCP_COMPLETE_MODE.
	MCPCompleteMode string
	// MCPAllowSkipClaim permits operating on tickets without claiming them. Env: MCP_ALLOW_SKIP_CLAIM.
	MCPAllowSkipClaim bool
	// MCPClaimWorker is the worker name stamped when claiming tickets. Env: MCP_CLAIM_WORKER.
	MCPClaimWorker string
	// MCPCommentAuthor is the author name stamped on tool-generated comments. Env: MCP_COMMENT_AUTHOR.
	MCPCommentAuthor string
	// MCPRateLimit caps MCP calls per minute. Env: MCP_RATE_LIMIT.
	MCPRateLimit int
	// LogLevel sets the log verbosity. Env: LOG_LEVEL.
	LogLevel string
}

// Default values applied when the corresponding optional environment
// variable is unset or empty.
const (
	defaultBindAddrs     = "127.0.0.1:9130"
	defaultKanbanBaseURL = "http://127.0.0.1:9119/api/plugins/kanban/"
	defaultKanbanBoard   = "hermes-agent"
	defaultHermesBin     = "hermes"
	defaultCompleteMode  = "review"
	defaultClaimWorker   = "opencode-remote"
	defaultCommentAuthor = "opencode-remote"
	defaultRateLimit     = 60
	defaultLogLevel      = "info"

	minBearerTokenLen = 16
)

// Load reads configuration from the environment, applying defaults for
// unset optional variables and validating required ones.
//
// KANBAN_USERNAME, KANBAN_PASSWORD, and MCP_BEARER_TOKEN are required;
// MCP_BEARER_TOKEN must also be at least 16 characters long.
// MCP_COMPLETE_MODE must be either "review" or "done". Any error
// names the offending environment variable.
func Load() (*Config, error) {
	cfg := &Config{
		BindAddrs:          getEnv("BIND_ADDRS", defaultBindAddrs),
		KanbanBaseURL:      getEnv("KANBAN_BASE_URL", defaultKanbanBaseURL),
		KanbanUsername:     os.Getenv("KANBAN_USERNAME"),
		KanbanPassword:     os.Getenv("KANBAN_PASSWORD"),
		MCPBearerToken:     os.Getenv("MCP_BEARER_TOKEN"),
		KanbanDefaultBoard: getEnv("KANBAN_DEFAULT_BOARD", defaultKanbanBoard),
		HermesBin:          getEnv("HERMES_BIN", defaultHermesBin),
		MCPCompleteMode:    getEnv("MCP_COMPLETE_MODE", defaultCompleteMode),
		MCPClaimWorker:     getEnv("MCP_CLAIM_WORKER", defaultClaimWorker),
		MCPCommentAuthor:   getEnv("MCP_COMMENT_AUTHOR", defaultCommentAuthor),
		LogLevel:           getEnv("LOG_LEVEL", defaultLogLevel),
	}

	if cfg.KanbanUsername == "" {
		return nil, fmt.Errorf("environment variable KANBAN_USERNAME is required")
	}
	if cfg.KanbanPassword == "" {
		return nil, fmt.Errorf("environment variable KANBAN_PASSWORD is required")
	}
	if cfg.MCPBearerToken == "" {
		return nil, fmt.Errorf("environment variable MCP_BEARER_TOKEN is required")
	}
	if len(cfg.MCPBearerToken) < minBearerTokenLen {
		return nil, fmt.Errorf("environment variable MCP_BEARER_TOKEN must be at least %d characters", minBearerTokenLen)
	}

	if v, ok := os.LookupEnv("MCP_ALLOW_SKIP_CLAIM"); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("environment variable MCP_ALLOW_SKIP_CLAIM must be a boolean: %w", err)
		}
		cfg.MCPAllowSkipClaim = b
	}

	if v, ok := os.LookupEnv("MCP_RATE_LIMIT"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("environment variable MCP_RATE_LIMIT must be an integer: %w", err)
		}
		cfg.MCPRateLimit = n
	} else {
		cfg.MCPRateLimit = defaultRateLimit
	}

	if cfg.MCPCompleteMode != "review" && cfg.MCPCompleteMode != "done" {
		return nil, fmt.Errorf("environment variable MCP_COMPLETE_MODE must be %q or %q, got %q",
			"review", "done", cfg.MCPCompleteMode)
	}

	return cfg, nil
}

// getEnv returns the value of key, or def when the variable is unset or
// set to an empty string.
func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// String renders every field except the secrets KANBAN_PASSWORD and
// MCP_BEARER_TOKEN, which are always shown as "***". The real secret
// values never appear in the output.
func (c *Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "BindAddrs=%s\n", c.BindAddrs)
	fmt.Fprintf(&b, "KanbanBaseURL=%s\n", c.KanbanBaseURL)
	fmt.Fprintf(&b, "KanbanUsername=%s\n", c.KanbanUsername)
	fmt.Fprintf(&b, "KanbanPassword=***\n")
	fmt.Fprintf(&b, "MCPBearerToken=***\n")
	fmt.Fprintf(&b, "KanbanDefaultBoard=%s\n", c.KanbanDefaultBoard)
	fmt.Fprintf(&b, "HermesBin=%s\n", c.HermesBin)
	fmt.Fprintf(&b, "MCPCompleteMode=%s\n", c.MCPCompleteMode)
	fmt.Fprintf(&b, "MCPAllowSkipClaim=%t\n", c.MCPAllowSkipClaim)
	fmt.Fprintf(&b, "MCPClaimWorker=%s\n", c.MCPClaimWorker)
	fmt.Fprintf(&b, "MCPCommentAuthor=%s\n", c.MCPCommentAuthor)
	fmt.Fprintf(&b, "MCPRateLimit=%d\n", c.MCPRateLimit)
	fmt.Fprintf(&b, "LogLevel=%s\n", c.LogLevel)
	return b.String()
}

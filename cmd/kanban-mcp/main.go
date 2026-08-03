// Command kanban-mcp is the HTTP entrypoint for the kanban-mcp service:
// an MCP Streamable HTTP server (official go-sdk) mounted at /mcp,
// guarded by a static bearer token and a per-client-IP rate limiter,
// backed by the Hermes kanban REST API.
//
// Routes:
//
//	GET /healthz — unauthenticated and rate-limit exempt (liveness only)
//	POST /mcp    — MCP Streamable HTTP endpoint (authenticated, rate-limited)
//	GET  /mcp    — protected like POST (SSE stream resume)
//
// Configuration is env-only (see internal/config). The bearer token and
// the kanban password are never logged; per-tool-call log lines carry
// tool, board, duration_ms, and outcome.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/experimance/kanban-mcp/internal/config"
	"github.com/experimance/kanban-mcp/internal/httpauth"
	"github.com/experimance/kanban-mcp/internal/kanban"
	"github.com/experimance/kanban-mcp/internal/mcptools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is injected at build time via
// -ldflags "-X main.version=<tag>"; local builds report "dev".
var version = "dev"

const shutdownTimeout = 15 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	logger := newLogger(os.Getenv("LOG_LEVEL"))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", "error", err)
		os.Exit(1)
	}

	client := kanban.New(cfg.KanbanUsername, cfg.KanbanPassword)
	mux := newMux(cfg, logger, client)

	servers, err := startServers(cfg.BindAddrs, mux, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}

	// cfg.String() redacts KANBAN_PASSWORD and MCP_BEARER_TOKEN.
	logger.Info("startup complete", "version", version, "config", cfg.String())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("signal received, shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(shutdownCtx); err != nil {
				logger.Error("shutdown", "addr", s.Addr, "error", err)
			}
		}(srv)
	}
	wg.Wait()
	logger.Info("shutdown complete")
}

// newMux builds the full HTTP surface: /healthz open and rate-limit
// exempt, /mcp behind the bearer middleware with the per-IP rate limiter
// wrapped inside it — auth runs before limiting, so a bad token gets a
// 401 even when the bucket is empty. The MCP server is created once and
// handed to the Streamable HTTP handler's getServer closure, which is
// the canonical single-server wiring from the design.
//
// lister backs the board_list tool; the real *kanban.Client is passed in
// production, tests substitute an in-memory fake.
func newMux(cfg *config.Config, logger *slog.Logger, lister mcptools.BoardLister) http.Handler {
	rl := httpauth.NewRateLimiter(cfg.MCPRateLimit, httpauth.DefaultBurst)

	srv := mcp.NewServer(&mcp.Implementation{Name: "kanban-mcp", Version: version}, nil)
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	mcp.AddTool(srv, mcptools.BoardListTool(),
		logToolCall(logger, "board_list", cfg.KanbanDefaultBoard,
			mcptools.BoardList(lister, cfg.KanbanDefaultBoard)))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/mcp", httpauth.Bearer(cfg.MCPBearerToken, rl.Wrap(h)))
	return mux
}

// logToolCall wraps a typed MCP tool handler so every call emits one
// structured JSON log line with tool, board, duration_ms, and outcome.
//
// outcome is one of:
//   - "ok" — the handler returned a result with a live request context
//   - "tool_error" — the handler returned an error the SDK will surface
//     as an isError tool result (a recoverable backend failure)
//   - "transport_error" — the request context was done by the time the
//     handler returned (client disconnect or deadline), so the result
//     cannot be delivered and the failure is transport-level
//
// The bearer token, the kanban password, and request bodies are never
// logged: the wrapper only ever emits the four fixed fields above.
func logToolCall[In, Out any](logger *slog.Logger, tool, board string, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		result, out, err := h(ctx, req, in)
		outcome := "ok"
		switch {
		case ctx.Err() != nil:
			outcome = "transport_error"
		case err != nil:
			outcome = "tool_error"
		}
		logger.Info("tool call",
			"tool", tool,
			"board", board,
			"duration_ms", time.Since(start).Milliseconds(),
			"outcome", outcome,
		)
		return result, out, err
	}
}

// startServers binds one listener per comma-separated address and serves
// the shared mux on each in its own goroutine. On any bind failure it
// returns the error; the process exits rather than serving a partial set.
func startServers(bindAddrs string, mux http.Handler, logger *slog.Logger) ([]*http.Server, error) {
	var servers []*http.Server
	for _, addr := range strings.Split(bindAddrs, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		srv := &http.Server{Addr: addr, Handler: mux}
		servers = append(servers, srv)
		go func(s *http.Server, l net.Listener) {
			if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("serve", "addr", s.Addr, "error", err)
			}
		}(srv, ln)
		logger.Info("listening", "addr", addr)
	}
	if len(servers) == 0 {
		return nil, errors.New("no listen addresses in BIND_ADDRS")
	}
	return servers, nil
}

// newLogger returns a slog logger writing JSON to stderr at the level
// named by LOG_LEVEL (debug|info|warn|error). An unrecognized level
// falls back to info with a warning.
func newLogger(level string) *slog.Logger {
	lvl, known := parseLogLevel(level)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if !known {
		logger.Warn("unknown LOG_LEVEL, falling back to info", "level", level)
	}
	return logger
}

func parseLogLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

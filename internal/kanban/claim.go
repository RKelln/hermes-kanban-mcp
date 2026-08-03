package kanban

// This file is the hardened CLI shell-out bridge: Claim and BlockTyped
// exec the hermes CLI with validated argv, a scrubbed environment, and a
// bounded timeout. Nothing in this package ever invokes a shell, reads
// Hermes state, or touches the kanban database directly.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Command constants. cmdTimeout bounds every shell-out; a caller-supplied
// context with a shorter deadline wins. maxOut caps each captured stream.
const (
	cmdTimeout = 30 * time.Second
	maxOut     = 4 << 10 // 4 KiB per stream

	// idRe/boardRe/kindRe are anchored so nothing can smuggle extra argv
	// into the CLI: spaces, shell metacharacters, and path traversal all
	// fail before anything is exec'd.
	idReStr    = `^[A-Za-z0-9._-]{1,64}$`
	boardReStr = `^[a-z0-9-]{1,64}$`
	kindReStr  = `^[a-z_]{1,32}$`
)

var (
	idRe    = regexp.MustCompile(idReStr)
	boardRe = regexp.MustCompile(boardReStr)
	kindRe  = regexp.MustCompile(kindReStr)
)

// hermes CLI location, resolved once per process and cached. binMu guards
// the cache; tests reset it to install the testdata fake.
var (
	binMu     sync.Mutex
	binCached bool
	binPath   string
	binOK     bool
)

// resolveBin locates the hermes CLI exactly once per process: HERMES_BIN
// if set (validated as executable via LookPath), otherwise the first
// "hermes" found on PATH. Every call shares the cached result, so a
// misconfigured start is reported consistently instead of re-probing per
// request.
func resolveBin() (path string, ok bool) {
	binMu.Lock()
	defer binMu.Unlock()
	if binCached {
		return binPath, binOK
	}
	binCached = true
	if want := os.Getenv("HERMES_BIN"); want != "" {
		if p, err := exec.LookPath(want); err == nil {
			binPath, binOK = p, true
		} else {
			binPath, binOK = want, false
		}
		return binPath, binOK
	}
	if p, err := exec.LookPath("hermes"); err == nil {
		binPath, binOK = p, true
	} else {
		binPath, binOK = "hermes", false
	}
	return binPath, binOK
}

// resetBinCache clears the cached CLI path so the next call re-resolves.
// Only the tests use it, to point the process at the testdata fake.
func resetBinCache() {
	binMu.Lock()
	binCached = false
	binMu.Unlock()
}

// ResetCLIBinCacheForTests clears the cached CLI path so the next call
// re-resolves HERMES_BIN. The MCP tool-layer tests use it to switch
// between the testdata fake and a deliberately missing binary; no
// production code calls it.
func ResetCLIBinCacheForTests() { resetBinCache() }

// cliNotFoundError is the stable tool error returned whenever the hermes
// CLI cannot be located. The "claim unavailable:" / "block unavailable:"
// prefix is what the MCP layer maps to KindUnavailable.
func cliNotFoundError(verb, path string) error {
	return fmt.Errorf("%s unavailable: hermes CLI not found at %s (set HERMES_BIN)", verb, path)
}

// CLIBinUnavailable returns the stable "unavailable" tool error when the
// hermes CLI cannot be resolved for the given verb ("claim" or "block"),
// or nil when it can. The MCP tool layer calls it before any preflight
// or exec so a missing HERMES_BIN fails fast on every call, and to pick
// between the CLI block path and the REST fallback. The result is the
// same error Claim/BlockTyped would return, so the two layers can never
// disagree about availability.
func CLIBinUnavailable(verb string) error {
	bin, ok := resolveBin()
	if ok {
		return nil
	}
	return cliNotFoundError(verb, bin)
}

// validateTarget enforces the task-id and board shapes before anything is
// exec'd. Both are anchored regexes, so attempts like "a b", "a;rm -rf /",
// or "../x" are rejected outright and never reach argv.
func validateTarget(id, board string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid task id %q (must match %s)", id, idReStr)
	}
	if !boardRe.MatchString(board) {
		return fmt.Errorf("invalid board %q (must match %s)", board, boardReStr)
	}
	return nil
}

// Claim atomically claims a kanban task by shelling out to
// `hermes kanban --board <board> claim <id>`.
//
// worker is accepted for API compatibility with the MCP layer but is not
// passed to the CLI: as of 2026-08 the real `hermes kanban claim` defines
// no assignee/worker flag (only --ttl), and inventing one is forbidden.
// When the CLI grows an assignee flag, plumb worker through here.
//
// stdout and stderr are the (4 KiB-capped) CLI streams; err is non-nil on
// argument rejection, missing binary, timeout, or non-zero exit.
func Claim(ctx context.Context, id, board, worker string) (stdout, stderr string, err error) {
	if err := validateTarget(id, board); err != nil {
		return "", "", fmt.Errorf("claim: %w", err)
	}
	bin, ok := resolveBin()
	if !ok {
		return "", "", cliNotFoundError("claim", bin)
	}
	// --board only parses before the verb in the real CLI (probed
	// 2026-08); after the subcommand argparse rejects it.
	argv := []string{"kanban", "--board", board, "claim", id}
	return run(ctx, bin, argv, "claim")
}

// BlockTyped blocks (or re-blocks) a kanban task with a typed reason by
// shelling out to `hermes kanban --board <board> block <id> [reason]
// [--kind <kind>]`.
//
// The flag layout matches the real CLI (probed 2026-08): there is no
// --reason flag (reason is positional and must precede --kind, or
// argparse reports it as unrecognized), --kind accepts capability |
// dependency | needs_input | transient, and --board goes before the verb.
// Empty kind and reason are omitted, which is the CLI's generic block.
func BlockTyped(ctx context.Context, id, board, kind, reason string) (stdout, stderr string, err error) {
	if err := validateTarget(id, board); err != nil {
		return "", "", fmt.Errorf("block: %w", err)
	}
	if kind != "" && !kindRe.MatchString(kind) {
		return "", "", fmt.Errorf("block: invalid kind %q (must match %s)", kind, kindReStr)
	}
	bin, ok := resolveBin()
	if !ok {
		return "", "", cliNotFoundError("block", bin)
	}
	argv := []string{"kanban", "--board", board, "block", id}
	if reason != "" {
		argv = append(argv, reason)
	}
	if kind != "" {
		argv = append(argv, "--kind", kind)
	}
	return run(ctx, bin, argv, "block")
}

// cmdContext bounds a command at cmdTimeout unless the caller's deadline
// is sooner. This is what keeps a hung CLI from wedging the MCP server.
func cmdContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < cmdTimeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, cmdTimeout)
}

// scrubbedEnv returns the child environment: only PATH, HOME, LANG, and
// HERMES_* variables pass through. Kanban credentials (KANBAN_PASSWORD,
// MCP_BEARER_TOKEN) and anything else the server was launched with are
// deliberately dropped — the CLI subprocess must never inherit secrets.
func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case key == "PATH" || key == "HOME" || key == "LANG":
			env = append(env, kv)
		case strings.HasPrefix(key, "HERMES_"):
			env = append(env, kv)
		}
	}
	return env
}

// run executes the hermes CLI with a scrubbed environment and captures
// stdout/stderr, capped at maxOut bytes each. On non-zero exit the error
// carries the exit status and the stderr tail; on context expiry it wraps
// the context error so errors.Is(err, context.DeadlineExceeded) works.
func run(ctx context.Context, bin string, argv []string, verb string) (stdout, stderr string, err error) {
	ctx, cancel := cmdContext(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = scrubbedEnv()

	var out, errOut bytes.Buffer
	cmd.Stdout = &capWriter{w: &out, max: maxOut}
	cmd.Stderr = &capWriter{w: &errOut, max: maxOut}

	if runErr := cmd.Run(); runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out.String(), errOut.String(), fmt.Errorf("hermes kanban %s: %w", verb, ctxErr)
		}
		if errText := strings.TrimSpace(errOut.String()); errText != "" {
			return out.String(), errOut.String(), fmt.Errorf("hermes kanban %s failed: %w: %s", verb, runErr, errText)
		}
		return out.String(), errOut.String(), fmt.Errorf("hermes kanban %s failed: %w", verb, runErr)
	}
	return out.String(), errOut.String(), nil
}

// capWriter is an io.Writer that keeps the first max bytes written and
// discards the rest without ever reporting a short write (mirroring
// io.Discard), so exec's pipe copiers stay happy past 4 KiB.
type capWriter struct {
	w   *bytes.Buffer
	max int
	n   int
}

func (c *capWriter) Write(p []byte) (int, error) {
	orig := len(p)
	if c.n < c.max {
		if room := c.max - c.n; len(p) > room {
			p = p[:room]
		}
		m, _ := c.w.Write(p)
		c.n += m
	}
	return orig, nil
}

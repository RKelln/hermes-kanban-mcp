package kanban

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFake points HERMES_BIN at the testdata fake CLI and forces the
// process to re-resolve it on the next call.
func installFake(t *testing.T) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "fake-hermes.sh"))
	if err != nil {
		t.Fatalf("resolve fake bin: %v", err)
	}
	t.Setenv("HERMES_BIN", abs)
	resetBinCache()
}

// breakBin points HERMES_BIN at a nonexistent binary so the "CLI not
// found" path fires.
func breakBin(t *testing.T) {
	t.Helper()
	t.Setenv("HERMES_BIN", "/nonexistent/hermes-cli")
	resetBinCache()
}

// shortCtx returns a context that expires quickly, for the timeout row.
func shortCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestClaim(t *testing.T) {
	installFake(t)
	// The caller's own MCP_CLAIM_TTL_SECONDS must not decide what this test
	// asserts: empty is treated as unset by ResolveClaimTTL.
	t.Setenv(ClaimTTLEnv, "")

	t.Run("exit 0", func(t *testing.T) {
		stdout, stderr, err := Claim(context.Background(), "task-ok", "default", "")
		if err != nil {
			t.Fatalf("Claim() error = %v, want nil", err)
		}
		if stdout != "claimed task-ok\n" {
			t.Errorf("stdout = %q, want %q", stdout, "claimed task-ok\n")
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("exit 1 with stderr", func(t *testing.T) {
		stdout, stderr, err := Claim(context.Background(), "task-fail", "default", "")
		if err == nil {
			t.Fatal("Claim() error = nil, want exit-status error")
		}
		for _, want := range []string{"hermes kanban claim failed", "exit status 1", "error: task already claimed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want substring %q", err, want)
			}
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if stderr != "error: task already claimed\n" {
			t.Errorf("stderr = %q, want %q", stderr, "error: task already claimed\n")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		stdout, _, err := Claim(shortCtx(t, 150*time.Millisecond), "task-hang", "default", "")
		if err == nil {
			t.Fatal("Claim() error = nil, want deadline exceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
	})

	t.Run("binary missing", func(t *testing.T) {
		breakBin(t)
		stdout, stderr, err := Claim(context.Background(), "task-ok", "default", "")
		want := "claim unavailable: hermes CLI not found at /nonexistent/hermes-cli (set HERMES_BIN)"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want exactly %q", err, want)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("stdout/stderr = %q/%q, want empty", stdout, stderr)
		}
	})

	t.Run("4 KiB cap", func(t *testing.T) {
		installFake(t)
		stdout, stderr, err := Claim(context.Background(), "task-big", "default", "")
		if err != nil {
			t.Fatalf("Claim() error = %v, want nil", err)
		}
		if len(stdout) != 4096 || len(stderr) != 4096 {
			t.Errorf("streams = %d/%d bytes, want 4096/4096", len(stdout), len(stderr))
		}
		if !strings.HasPrefix(stdout, "0123456789") {
			t.Errorf("stdout = %q..., want 0123456789 prefix", stdout[:32])
		}
	})
}

func TestResolveClaimTTL(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		wantTTL    int64
		wantSource string
	}{
		{"unset falls back to the documented default", "", DefaultClaimTTLSeconds, claimTTLSourceDefault},
		{"whitespace counts as unset", "   ", DefaultClaimTTLSeconds, claimTTLSourceDefault},
		{"operator value is carried", "7200", 7200, claimTTLSourceEnv},
		{"surrounding whitespace is trimmed", " 7200\n", 7200, claimTTLSourceEnv},
		{"zero is not a TTL", "0", DefaultClaimTTLSeconds, claimTTLSourceInvalid},
		{"negative is not a TTL", "-60", DefaultClaimTTLSeconds, claimTTLSourceInvalid},
		{"a duration string is not a TTL", "15m", DefaultClaimTTLSeconds, claimTTLSourceInvalid},
		{"a float is not a TTL", "3600.5", DefaultClaimTTLSeconds, claimTTLSourceInvalid},
		{"overflow is not a TTL", "99999999999999999999", DefaultClaimTTLSeconds, claimTTLSourceInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(ClaimTTLEnv, tt.env)
			got := ResolveClaimTTL()
			if got.Seconds != tt.wantTTL || got.Source != tt.wantSource {
				t.Errorf("ResolveClaimTTL() = %+v, want {Seconds:%d Source:%q}", got, tt.wantTTL, tt.wantSource)
			}
		})
	}
}

func TestClaimArgvLayout(t *testing.T) {
	installFake(t)

	// The default TTL is passed EXPLICITLY: the CLI's own 900s default is
	// what let external-lane claims die mid-run, and the bridge's argv is
	// the only place the value can come from (the kernel's env knob loses
	// to the CLI's call-site default).
	// worker must NOT reach argv: the real CLI defines no assignee flag.
	t.Setenv(ClaimTTLEnv, "")
	stdout, _, err := Claim(context.Background(), "task-echo", "default", "alice")
	if err != nil {
		t.Fatalf("Claim() error = %v, want nil", err)
	}
	want := "argv: kanban --board default claim --ttl 14400 task-echo\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	// The knob reaches argv, not just the response.
	t.Setenv(ClaimTTLEnv, "7200")
	stdout, _, err = Claim(context.Background(), "task-echo", "default", "")
	if err != nil {
		t.Fatalf("Claim() error = %v, want nil", err)
	}
	want = "argv: kanban --board default claim --ttl 7200 task-echo\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestClaimWithTTLNeverEmitsNonPositive is the argv guard.
// `hermes kanban claim --ttl 0` parses (the CLI does not validate the value)
// and would hand the dispatcher a claim that is stale on arrival, so the
// shell-out repairs it to the default instead of passing it through.
func TestClaimWithTTLNeverEmitsNonPositive(t *testing.T) {
	installFake(t)

	for _, ttl := range []ClaimTTLSettings{
		{Seconds: 0},
		{Seconds: -1, Source: ClaimTTLEnv},
	} {
		stdout, _, err := ClaimWithTTL(context.Background(), "task-echo", "default", "", ttl)
		if err != nil {
			t.Fatalf("ClaimWithTTL(%+v) error = %v, want nil", ttl, err)
		}
		want := "argv: kanban --board default claim --ttl 14400 task-echo\n"
		if stdout != want {
			t.Errorf("ClaimWithTTL(%+v) stdout = %q, want %q", ttl, stdout, want)
		}
	}
}

func TestClaimInjection(t *testing.T) {
	installFake(t) // if validation leaks through, the fake would run

	badIDs := []string{"a b", "a;rm -rf /", "../x", "", "t_x\n--board", "`id`", "t" + strings.Repeat("a", 64)}
	badBoards := []string{"BOGUS", "a b", "../x", "", "x_y"}

	for _, id := range badIDs {
		t.Run("id "+strconvQuote(id), func(t *testing.T) {
			stdout, stderr, err := Claim(context.Background(), id, "default", "")
			if err == nil || !strings.Contains(err.Error(), "invalid task id") {
				t.Errorf("err = %v, want invalid-task-id error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}
	for _, board := range badBoards {
		t.Run("board "+strconvQuote(board), func(t *testing.T) {
			stdout, stderr, err := Claim(context.Background(), "task-ok", board, "")
			if err == nil || !strings.Contains(err.Error(), "invalid board") {
				t.Errorf("err = %v, want invalid-board error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}
}

func TestBlockTyped(t *testing.T) {
	installFake(t)

	t.Run("exit 0 with kind and reason", func(t *testing.T) {
		stdout, stderr, err := BlockTyped(context.Background(), "task-ok", "default", "needs_input", "test reason")
		if err != nil {
			t.Fatalf("BlockTyped() error = %v, want nil", err)
		}
		if stdout != "claimed task-ok\n" {
			t.Errorf("stdout = %q, want %q", stdout, "claimed task-ok\n")
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("exit 1 with stderr", func(t *testing.T) {
		stdout, stderr, err := BlockTyped(context.Background(), "task-fail", "default", "needs_input", "why")
		if err == nil {
			t.Fatal("BlockTyped() error = nil, want exit-status error")
		}
		for _, want := range []string{"hermes kanban block failed", "exit status 1", "error: task already claimed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, want substring %q", err, want)
			}
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if stderr != "error: task already claimed\n" {
			t.Errorf("stderr = %q, want %q", stderr, "error: task already claimed\n")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		stdout, _, err := BlockTyped(shortCtx(t, 150*time.Millisecond), "task-hang", "default", "", "")
		if err == nil {
			t.Fatal("BlockTyped() error = nil, want deadline exceeded")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want errors.Is(err, context.DeadlineExceeded)", err)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
	})

	t.Run("binary missing", func(t *testing.T) {
		breakBin(t)
		stdout, stderr, err := BlockTyped(context.Background(), "task-ok", "default", "", "")
		want := "block unavailable: hermes CLI not found at /nonexistent/hermes-cli (set HERMES_BIN)"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want exactly %q", err, want)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("stdout/stderr = %q/%q, want empty", stdout, stderr)
		}
	})
}

func TestBlockTypedArgvLayout(t *testing.T) {
	installFake(t)

	tests := []struct {
		name   string
		kind   string
		reason string
		want   string
	}{
		{
			name:   "kind and reason, probed layout",
			kind:   "needs_input",
			reason: "test reason",
			// --board before the verb; reason positional BEFORE --kind
			// (argparse rejects reason after --kind as unrecognized).
			want: "argv: kanban --board default block task-echo test reason --kind needs_input\n",
		},
		{
			name: "generic block, no kind or reason",
			want: "argv: kanban --board default block task-echo\n",
		},
		{
			name:   "reason only",
			reason: "just because",
			want:   "argv: kanban --board default block task-echo just because\n",
		},
		{
			name: "kind only",
			kind: "dependency",
			want: "argv: kanban --board default block task-echo --kind dependency\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := BlockTyped(context.Background(), "task-echo", "default", tt.kind, tt.reason)
			if err != nil {
				t.Fatalf("BlockTyped() error = %v, want nil", err)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
		})
	}
}

func TestBlockTypedInjection(t *testing.T) {
	installFake(t) // if validation leaks through, the fake would run

	for _, id := range []string{"a b", "a;rm -rf /", "../x", ""} {
		t.Run("id "+strconvQuote(id), func(t *testing.T) {
			stdout, stderr, err := BlockTyped(context.Background(), id, "default", "", "")
			if err == nil || !strings.Contains(err.Error(), "invalid task id") {
				t.Errorf("err = %v, want invalid-task-id error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}

	for _, kind := range []string{"x;rm -rf /", "NeedsInput", "transient!", strings.Repeat("k", 33)} {
		t.Run("kind "+strconvQuote(kind), func(t *testing.T) {
			stdout, stderr, err := BlockTyped(context.Background(), "task-ok", "default", kind, "")
			if err == nil || !strings.Contains(err.Error(), "invalid kind") {
				t.Errorf("err = %v, want invalid-kind error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}
}

// strconvQuote renders a test-case value legibly in a subtest name.
func strconvQuote(s string) string {
	if s == "" {
		return "(empty)"
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "/", "_"), " ", "_")
}

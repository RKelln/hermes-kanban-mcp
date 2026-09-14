package kanban

// review_test.go covers RequestReview: the argv layout (including the
// --flag=value form that protects a leading-dash summary from being
// parsed as an option), the force decision, the standard failure matrix
// (exit 1, timeout, missing binary), and injection rejection.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRequestReview(t *testing.T) {
	installFake(t)

	t.Run("exit 0", func(t *testing.T) {
		stdout, stderr, err := RequestReview(context.Background(), "task-ok", "default", "handoff text", "alice", false)
		if err != nil {
			t.Fatalf("RequestReview() error = %v, want nil", err)
		}
		if stdout != "claimed task-ok\n" {
			t.Errorf("stdout = %q, want %q", stdout, "claimed task-ok\n")
		}
		if stderr != "" {
			t.Errorf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("exit 1 with stderr", func(t *testing.T) {
		stdout, stderr, err := RequestReview(context.Background(), "task-fail", "default", "s", "alice", false)
		if err == nil {
			t.Fatal("RequestReview() error = nil, want exit-status error")
		}
		for _, want := range []string{"hermes kanban request-review failed", "exit status 1", "error: task already claimed"} {
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
		stdout, _, err := RequestReview(shortCtx(t, 150*time.Millisecond), "task-hang", "default", "s", "alice", false)
		if err == nil {
			t.Fatal("RequestReview() error = nil, want deadline exceeded")
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
		stdout, stderr, err := RequestReview(context.Background(), "task-ok", "default", "s", "alice", false)
		want := "request-review unavailable: hermes CLI not found at /nonexistent/hermes-cli (set HERMES_BIN)"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want exactly %q", err, want)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("stdout/stderr = %q/%q, want empty", stdout, stderr)
		}
	})

	t.Run("4 KiB cap", func(t *testing.T) {
		installFake(t)
		stdout, stderr, err := RequestReview(context.Background(), "task-big", "default", "s", "alice", false)
		if err != nil {
			t.Fatalf("RequestReview() error = %v, want nil", err)
		}
		if len(stdout) != 4096 || len(stderr) != 4096 {
			t.Errorf("streams = %d/%d bytes, want 4096/4096", len(stdout), len(stderr))
		}
	})
}

// TestRequestReviewArgvLayout pins the exact argv. --board must precede
// the verb (argparse rejects it after), and --summary/--reviewer use the
// `=` form so a value starting with a dash is a value, never an option.
func TestRequestReviewArgvLayout(t *testing.T) {
	installFake(t)

	tests := []struct {
		name     string
		summary  string
		reviewer string
		force    bool
		want     string
	}{
		{
			name:     "summary and reviewer, ready ticket",
			summary:  "why",
			reviewer: "alice",
			want:     "argv: kanban --board default request-review task-echo --summary=why --reviewer=alice\n",
		},
		{
			name:     "running ticket adds force last",
			summary:  "why",
			reviewer: "alice",
			force:    true,
			want:     "argv: kanban --board default request-review task-echo --summary=why --reviewer=alice --force\n",
		},
		{
			name: "everything omitted",
			want: "argv: kanban --board default request-review task-echo\n",
		},
		{
			// The load-bearing case: a leading-dash summary must be
			// carried as a VALUE. With the space form the CLI's
			// argparse would read --force as an option and reject the
			// call as "expected one argument".
			name:    "leading-dash summary stays a value",
			summary: "--force --board evil",
			want:    "argv: kanban --board default request-review task-echo --summary=--force --board evil\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := RequestReview(context.Background(), "task-echo", "default", tt.summary, tt.reviewer, tt.force)
			if err != nil {
				t.Fatalf("RequestReview() error = %v, want nil", err)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
		})
	}
}

func TestRequestReviewInjection(t *testing.T) {
	installFake(t) // if validation leaks through, the fake would run

	for _, id := range []string{"a b", "a;rm -rf /", "../x", "", "t_x\n--board", "`id`"} {
		t.Run("id "+strconvQuote(id), func(t *testing.T) {
			stdout, stderr, err := RequestReview(context.Background(), id, "default", "s", "alice", false)
			if err == nil || !strings.Contains(err.Error(), "invalid task id") {
				t.Errorf("err = %v, want invalid-task-id error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}

	for _, board := range []string{"BOGUS", "a b", "../x", "", "x_y"} {
		t.Run("board "+strconvQuote(board), func(t *testing.T) {
			stdout, stderr, err := RequestReview(context.Background(), "task-ok", board, "s", "alice", false)
			if err == nil || !strings.Contains(err.Error(), "invalid board") {
				t.Errorf("err = %v, want invalid-board error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}

	for _, reviewer := range []string{"a b", "alice;rm -rf /", "alice/x", "alice\n--force", strings.Repeat("r", 65)} {
		t.Run("reviewer "+strconvQuote(reviewer), func(t *testing.T) {
			stdout, stderr, err := RequestReview(context.Background(), "task-ok", "default", "s", reviewer, false)
			if err == nil || !strings.Contains(err.Error(), "invalid reviewer") {
				t.Errorf("err = %v, want invalid-reviewer error", err)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("stdout/stderr = %q/%q, want empty (must not exec)", stdout, stderr)
			}
		})
	}

	t.Run("NUL in summary", func(t *testing.T) {
		_, _, err := RequestReview(context.Background(), "task-ok", "default", "a\x00b", "alice", false)
		if err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("err = %v, want NUL-byte rejection", err)
		}
	})

	t.Run("oversized summary", func(t *testing.T) {
		_, _, err := RequestReview(context.Background(), "task-ok", "default", strings.Repeat("x", maxSummaryBytes+1), "alice", false)
		if err == nil || !strings.Contains(err.Error(), "summary too long") {
			t.Errorf("err = %v, want summary-too-long rejection", err)
		}
	})

	t.Run("summary at the cap is accepted", func(t *testing.T) {
		if _, _, err := RequestReview(context.Background(), "task-ok", "default", strings.Repeat("x", maxSummaryBytes), "alice", false); err != nil {
			t.Errorf("RequestReview() error = %v, want nil at exactly maxSummaryBytes", err)
		}
	})
}

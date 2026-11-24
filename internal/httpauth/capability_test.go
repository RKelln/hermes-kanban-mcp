package httpauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testWriteToken = "write-token-ZYXWVUTS9876543210"

// TestTokensCapability pins the decision table the whole split rests on.
func TestTokensCapability(t *testing.T) {
	legacy := Tokens{Read: testToken}
	split := Tokens{Read: testToken, Write: testWriteToken}

	tests := []struct {
		name   string
		tokens Tokens
		auth   string
		want   Capability
		ok     bool
	}{
		{"legacy/read token is full", legacy, "Bearer " + testToken, CapabilityFull, true},
		{"legacy/write value unknown", legacy, "Bearer " + testWriteToken, CapabilityFull, false},
		{"split/read token is read", split, "Bearer " + testToken, CapabilityRead, true},
		{"split/write token is write", split, "Bearer " + testWriteToken, CapabilityWrite, true},
		{"split/lowercase scheme", split, "bearer " + testWriteToken, CapabilityWrite, true},
		{"split/unknown token", split, "Bearer nope", CapabilityFull, false},
		{"split/missing header", split, "", CapabilityFull, false},
		{"split/wrong scheme", split, "Basic " + testWriteToken, CapabilityFull, false},
		{"split/extra field", split, "Bearer " + testWriteToken + " extra", CapabilityFull, false},
		{"split/write token with trailing space", split, "Bearer " + testWriteToken + " ", CapabilityFull, false},
		{"empty config rejects everything", Tokens{}, "Bearer " + testToken, CapabilityFull, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.tokens.capability(tt.auth)
			if ok != tt.ok || got != tt.want {
				t.Errorf("capability(%q) = (%v, %v), want (%v, %v)", tt.auth, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestAllowsWrite pins which capabilities may call a write tool. CapabilityFull
// is the zero value, so an in-process caller keeps the legacy surface.
func TestAllowsWrite(t *testing.T) {
	tests := []struct {
		cap  Capability
		want bool
	}{
		{CapabilityFull, true},
		{CapabilityWrite, true},
		{CapabilityRead, false},
	}
	for _, tt := range tests {
		if got := tt.cap.AllowsWrite(); got != tt.want {
			t.Errorf("Capability(%d).AllowsWrite() = %v, want %v", tt.cap, got, tt.want)
		}
	}
}

// capabilityEchoHandler reports the capability the middleware recorded, so a
// test can prove the value actually rides the request context rather than
// being assumed.
func capabilityEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(CapabilityFromContext(r.Context()).String()))
	})
}

func TestBearerRecordsCapability(t *testing.T) {
	tests := []struct {
		name   string
		tokens Tokens
		auth   string
		want   string
	}{
		{"legacy single token", Tokens{Read: testToken}, "Bearer " + testToken, "full"},
		{"split read token", Tokens{Read: testToken, Write: testWriteToken}, "Bearer " + testToken, "read"},
		{"split write token", Tokens{Read: testToken, Write: testWriteToken}, "Bearer " + testWriteToken, "write"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Authorization", tt.auth)
			rec := httptest.NewRecorder()
			Bearer(tt.tokens, capabilityEchoHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != tt.want {
				t.Errorf("capability on context = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCapabilityFromPlainContextIsFull pins the documented default: a context
// the middleware never touched keeps the legacy full surface, which is what the
// in-process tests and a non-HTTP transport see.
func TestCapabilityFromPlainContextIsFull(t *testing.T) {
	if got := CapabilityFromContext(context.Background()); got != CapabilityFull {
		t.Errorf("CapabilityFromContext(context.Background()) = %v, want full", got)
	}
}

// TestSplitModeStillChallengesUnknownTokens proves the split did not soften the
// 401 contract: only the two configured values are admitted, and anything else
// keeps the byte-exact rejected shape.
func TestSplitModeStillChallengesUnknownTokens(t *testing.T) {
	split := Tokens{Read: testToken, Write: testWriteToken}
	for _, auth := range []string{"", "Bearer nope", "Bearer " + testToken[:len(testToken)-1] + "x"} {
		rec := doTokens(http.MethodPost, split, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("auth %q: status = %d, want 401", auth, rec.Code)
		}
		if got := rec.Body.String(); got != wantBody {
			t.Errorf("auth %q: body = %q, want %q", auth, got, wantBody)
		}
		if got := rec.Header().Get("X-Underside"); got != "" {
			t.Errorf("auth %q: underlying handler ran on a rejected request", auth)
		}
	}
}

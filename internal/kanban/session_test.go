package kanban

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewSessionClientStripsPluginMount verifies the login POST is sent to
// the dashboard ROOT (/auth/password-login), not under the plugin mount
// (/api/plugins/kanban/...), when baseURL carries the plugin prefix.
//
// Regression for the bug found by the live smoke (t_eec004f0): the old code
// TrimRight'ed the trailing slash BEFORE the HasSuffix check, so the
// slash-terminated pluginMount never matched and the login POST went to
// .../kanban/auth/password-login — a route behind the session middleware,
// which answers 401. The existing suite missed it because its httptest
// fakes override baseURL to bare origins.
func TestNewSessionClientStripsPluginMount(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// baseURL with the plugin mount suffix, exactly as the env example ships it.
	baseURL := srv.URL + "/api/plugins/kanban/"
	c, err := NewSessionClient(baseURL, "u", "p")
	if err != nil {
		t.Fatalf("NewSessionClient returned error: %v", err)
	}
	if c == nil {
		t.Fatal("NewSessionClient returned nil client")
	}
	if gotPath != "/auth/password-login" {
		t.Fatalf("login POST went to %q, want %q (plugin mount must be stripped)", gotPath, "/auth/password-login")
	}
}

// TestNewSessionClientBareOrigin keeps the bare-origin contract intact:
// a baseURL without the plugin mount is left untouched.
func TestNewSessionClientBareOrigin(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := NewSessionClient(srv.URL, "u", "p"); err != nil {
		t.Fatalf("NewSessionClient returned error: %v", err)
	}
	if gotPath != "/auth/password-login" {
		t.Fatalf("login POST went to %q, want %q", gotPath, "/auth/password-login")
	}
	// Confirm the strip helper behavior on a trailing-slash bare origin too.
	root := strings.TrimSuffix(strings.TrimRight(srv.URL+"/", "/"), strings.TrimSuffix(pluginMount, "/"))
	if root != srv.URL {
		t.Fatalf("bare-origin root = %q, want %q", root, srv.URL)
	}
}

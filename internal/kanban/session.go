package kanban

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// pluginMount is the kanban plugin's REST mount; the login route lives
// at the dashboard root, not under this prefix.
const pluginMount = "/api/plugins/kanban/"

// NewSessionClient logs in to the dashboard once and returns an
// *http.Client carrying the session cookies, ready for authenticated
// kanban REST calls. The login route lives at the dashboard root
// (/auth/password-login) — NOT under the plugin mount, which sits
// behind the session middleware — so the plugin prefix is stripped from
// baseURL before the login POST. Session cookies are stored in the jar
// and sent automatically on subsequent calls.
//
// The session TTL is 3600s with refresh tokens; a server that outlives
// the session will start receiving 401s, which the tool layer surfaces
// as auth errors. Proactive re-login on 401 is future work.
func NewSessionClient(baseURL, username, password string) (*http.Client, error) {
	root := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), strings.TrimSuffix(pluginMount, "/"))
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	payload, err := json.Marshal(map[string]string{
		"provider": "basic",
		"username": username,
		"password": password,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal login payload: %w", err)
	}
	resp, err := client.Post(root+"/auth/password-login", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("kanban login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kanban login failed: HTTP %d", resp.StatusCode)
	}
	return client, nil
}

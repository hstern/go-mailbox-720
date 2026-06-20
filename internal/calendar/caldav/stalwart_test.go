//go:build dockertest

package caldav

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Stalwart is a full groupware server that implements RFC 6638 CalDAV
// auto-scheduling natively (it advertises calendar-auto-schedule and performs
// iTIP/iMIP itself). It backs the native-scheduling gating e2e with a REAL
// scheduler, unlike the Radicale+OPTIONS-proxy fake.
const (
	stalwartImage = "stalwartlabs/stalwart:latest"
	stalwartCtr   = "mailbox-e2e-stalwart"
	stalwartAdmin = "admin"
	stalwartPass  = "adminpass"

	// stalwartUser is the mailbox; its login is stalwartUser@stalwartDomain and its
	// password must clear Stalwart's strength check, so it is a random-ish string.
	stalwartUser   = "me"
	stalwartDomain = "example.com"
	stalwartUserPW = "Zx9-Kp2v-Qm7w-Lt4r"

	// stalwartConfig is the entire on-disk config: in v0.16 config.json holds ONLY
	// the datastore (everything else — domains, accounts — lives in the store and
	// is provisioned over the management JMAP API). The root @type is the store.
	stalwartConfig = `{ "@type": "RocksDb", "path": "/opt/stalwart/data" }`
)

// startStalwart runs a Stalwart container, provisions a domain + an individual
// account over the management JMAP API, and returns the CalDAV base URL plus the
// account's login and password. The container is removed via t.Cleanup.
func startStalwart(t *testing.T) (base, login, pass string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := dir + "/config.json"
	if err := os.WriteFile(cfg, []byte(stalwartConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", stalwartCtr).Run()

	port := freePort(t)
	// Run as root so the server can create /opt/stalwart/data; pin the admin via
	// STALWART_RECOVERY_ADMIN so the management API is reachable without the
	// bootstrap web UI. The datastore-only config skips bootstrap and starts the
	// CalDAV/JMAP listeners immediately.
	out, err := exec.Command("docker", "run", "-d", "--name", stalwartCtr,
		"-u", "0",
		"-e", "STALWART_RECOVERY_ADMIN="+stalwartAdmin+":"+stalwartPass,
		"-v", cfg+":/etc/stalwart/config.json:ro",
		"-p", "127.0.0.1:"+port+":8080",
		stalwartImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run stalwart: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", stalwartCtr).Run() })

	root := "http://127.0.0.1:" + port
	waitStalwart(t, root)

	domainID := stalwartCreate(t, root, "x:Domain/set", "d", map[string]any{
		"name": stalwartDomain, "aliases": map[string]any{},
		"certificateManagement": map[string]any{"@type": "Manual"},
		"dkimManagement":        map[string]any{"@type": "Automatic"},
		"dnsManagement":         map[string]any{"@type": "Manual"},
		"subAddressing":         map[string]any{"@type": "Enabled"},
	})
	stalwartCreate(t, root, "x:Account/set", "u", map[string]any{
		"@type": "User", "name": stalwartUser, "domainId": domainID,
		"credentials": map[string]any{"0": map[string]any{
			"@type": "Password", "secret": stalwartUserPW,
			"otpAuth": nil, "expiresAt": nil, "allowedIps": map[string]any{},
		}},
		"memberGroupIds": map[string]any{},
		"roles":          map[string]any{"@type": "User"},
		"permissions":    map[string]any{"@type": "Inherit"},
		"quotas":         map[string]any{},
		// The login address (name@domain) is an explicit alias; without it the
		// account has no email address and cannot authenticate.
		"aliases":          map[string]any{"0": map[string]any{"enabled": true, "name": stalwartUser, "domainId": domainID}},
		"encryptionAtRest": map[string]any{"@type": "Disabled"},
	})

	login = stalwartUser + "@" + stalwartDomain
	// The CalDAV endpoint the adapter discovers from.
	return root + "/dav/cal/", login, stalwartUserPW
}

// waitStalwart polls the management JMAP session until the server is ready.
func waitStalwart(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, root+"/jmap/session", nil)
		req.SetBasicAuth(stalwartAdmin, stalwartPass)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("stalwart did not become ready")
}

// stalwartCreate issues a management JMAP <type>/set create call (urn:stalwart:jmap,
// x:-prefixed types) and returns the created object's id.
func stalwartCreate(t *testing.T, root, method, key string, obj map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"},
		"methodCalls": []any{[]any{method, map[string]any{"create": map[string]any{key: obj}}, "c1"}},
	})
	req, _ := http.NewRequest(http.MethodPost, root+"/jmap/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(stalwartAdmin, stalwartPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)

	var out struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.MethodResponses) == 0 {
		t.Fatalf("%s: bad response: %s", method, raw)
	}
	var call []json.RawMessage
	_ = json.Unmarshal(out.MethodResponses[0], &call)
	var args struct {
		Created map[string]struct {
			ID string `json:"id"`
		} `json:"created"`
		NotCreated json.RawMessage `json:"notCreated"`
	}
	_ = json.Unmarshal(call[1], &args)
	if c, ok := args.Created[key]; ok && c.ID != "" {
		return c.ID
	}
	t.Fatalf("%s did not create %q: %s", method, key, raw)
	return ""
}

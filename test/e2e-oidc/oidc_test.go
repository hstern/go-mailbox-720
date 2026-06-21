// Package e2e is a black-box vertical-slice integration test. For each IdP in the
// matrix (Kanidm, Zitadel) it stands up the real IdP and a real Dovecot IMAP +
// Radicale CalDAV server in containers, provisions an OAuth2 resource server, seeds
// a message and an event, and runs the real mailboxd wired to all three. It then
// asserts the whole path: an IdP-issued token is validated, the handler pulls the
// inbox/calendar, and GET /v1.0/me/messages|events returns the seeded data as Graph
// JSON (200) — while an unauthenticated request is rejected (401).
//
// Everything is driven over HTTP with plain Go + the docker CLI (no shell or Python
// scripts). The test self-skips when docker is unavailable; mailboxd is built from
// the parent module, which must have run `go generate ./internal/graph` first.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestOIDCEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	for _, p := range idps() {
		t.Run(p.name(), func(t *testing.T) {
			p.start(t)
			p.provision(t)

			// Real mail backend: Dovecot with one seeded message in the inbox.
			dovecotAddr := startDovecot(t)
			seedMessage(t, dovecotAddr, testMessage)

			// Real calendar backend: Radicale (CalDAV) with one seeded calendar + event.
			radicaleBase := startRadicale(t)
			seedCalendar(t, radicaleBase)

			base := startMailboxd(t, p, dovecotAddr, radicaleBase)
			token := p.mintToken(t)

			// No token is rejected by the middleware.
			if got := status(t, base+"/me/messages", ""); got != http.StatusUnauthorized {
				t.Errorf("unauthenticated request: status = %d, want 401", got)
			}

			// The full vertical slice: the IdP token is validated, the handler pulls
			// the inbox from Dovecot, and the seeded message comes back as Graph JSON.
			code, body := get(t, base+"/me/messages", token)
			if code != http.StatusOK {
				t.Fatalf("authenticated /me/messages: status = %d, body = %s", code, body)
			}
			var resp struct {
				Value []struct {
					Subject string `json:"subject"`
					From    struct {
						EmailAddress struct {
							Address string `json:"address"`
						} `json:"emailAddress"`
					} `json:"from"`
				} `json:"value"`
			}
			if err := json.Unmarshal(body, &resp); err != nil {
				t.Fatalf("decode response: %v (%s)", err, body)
			}
			if len(resp.Value) != 1 {
				t.Fatalf("got %d messages, want 1: %s", len(resp.Value), body)
			}
			if got := resp.Value[0].Subject; got != "Hello there" {
				t.Errorf("subject = %q, want %q", got, "Hello there")
			}
			if got := resp.Value[0].From.EmailAddress.Address; got != "alice@example.com" {
				t.Errorf("from = %q, want alice@example.com", got)
			}

			// The calendar vertical slice: the same token authorizes GET /me/events,
			// the handler resolves the principal's default (first) calendar over CalDAV
			// from Radicale, and the seeded event comes back as Graph JSON.
			ecode, ebody := get(t, base+"/me/events", token)
			if ecode != http.StatusOK {
				t.Fatalf("authenticated /me/events: status = %d, body = %s", ecode, ebody)
			}
			var eresp struct {
				Value []struct {
					Subject string `json:"subject"`
				} `json:"value"`
			}
			if err := json.Unmarshal(ebody, &eresp); err != nil {
				t.Fatalf("decode events response: %v (%s)", err, ebody)
			}
			if len(eresp.Value) != 1 {
				t.Fatalf("got %d events, want 1: %s", len(eresp.Value), ebody)
			}
			if got := eresp.Value[0].Subject; got != eventSummary {
				t.Errorf("event subject = %q, want %q", got, eventSummary)
			}
		})
	}
}

// startMailboxd builds and runs the server with auth enforced against the given IdP,
// wired to the IMAP mail backend and the CalDAV calendar backend, and returns its
// /v1.0 base URL.
func startMailboxd(t *testing.T, p idp, imapAddr, caldavURL string) string {
	t.Helper()
	bin := buildMailboxd(t)

	addr := freeAddr(t)
	args := append([]string{"-addr", addr}, authFlags(p)...)
	args = append(args,
		"-mail-imap-addr", imapAddr,
		"-mail-imap-username", dovecotUser,
		"-mail-imap-tls=false",
		"-cal-caldav-url", caldavURL,
		"-cal-caldav-username", radicaleUser,
	)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), authEnv(p)...)
	cmd.Env = append(cmd.Env,
		"MAILBOXD_IMAP_PASSWORD="+dovecotPass,
		"MAILBOXD_CALDAV_PASSWORD="+radicalePass,
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mailboxd: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	base := "http://" + addr + "/v1.0"
	waitFor(t, "mailboxd", 30*time.Second, func() bool {
		resp, err := http.Get(base + "/me/messages") // 401 once up (auth on, no token)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	})
	return base
}

// buildMailboxd compiles the server from the parent module into a temp binary.
func buildMailboxd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mailboxd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/mailboxd")
	build.Dir = "../.."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build mailboxd (did you run `go generate ./internal/graph`?): %v", err)
	}
	return bin
}

// status issues GET url (with an optional bearer token) and returns the status.
func status(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// get issues GET url with an optional bearer token and returns status + body.
func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// --- Dovecot (the mail backend) ---

const (
	dovecotImage = "dovecot/dovecot:2.3.21"
	dovecotCtr   = "mailbox-e2e-oidc-dovecot"
	dovecotUser  = "test"
	dovecotPass  = "testpass"

	testMessage = "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Hello there\r\n" +
		"Date: Wed, 11 Jun 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"This is the body of the message.\r\n"

	dovecotConf = `mail_location = maildir:/srv/mail/%u/Maildir
mail_uid = 1000
mail_gid = 1000
first_valid_uid = 1000
ssl = no
disable_plaintext_auth = no
auth_mechanisms = plain login
passdb {
  driver = static
  args = password=testpass
}
userdb {
  driver = static
  args = uid=1000 gid=1000 home=/srv/mail/%u
}
protocols = imap
service imap-login {
  inet_listener imap {
    port = 143
  }
}
listen = *
log_path = /dev/stderr
`
)

func startDovecot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "dovecot.conf")
	if err := os.WriteFile(conf, []byte(dovecotConf), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", dovecotCtr).Run()

	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	out, err := exec.Command("docker", "run", "-d", "--name", dovecotCtr,
		"-v", conf+":/etc/dovecot/dovecot.conf:ro",
		"-p", "127.0.0.1:"+port+":143",
		dovecotImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dovecot: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", dovecotCtr).Run() })

	waitFor(t, "dovecot", 30*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return false
		}
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		return strings.Contains(line, "OK")
	})
	return addr
}

// seedMessage appends raw to INBOX via a raw IMAP client.
func seedMessage(t *testing.T, addr, raw string) {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("seed dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(dovecotUser, dovecotPass).Wait(); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	ac := c.Append("INBOX", int64(len(raw)), &goimap.AppendOptions{})
	if _, err := ac.Write([]byte(raw)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := ac.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	_ = c.Logout().Wait()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitFor(t *testing.T, what string, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready within %s", what, timeout)
}

// run executes a command, failing the test (with output) on error.
func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

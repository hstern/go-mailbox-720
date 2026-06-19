//go:build dockertest

// Integration test for the IMAP adapter against a real Dovecot server in Docker.
// Build-tagged so the default `go test ./...` (which uses the in-process
// imapmemserver) stays fast and dependency-free; run with:
//
//	go test -tags dockertest ./internal/mail/imap/
//
// Self-skips when docker is unavailable.
package imap

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

const (
	dovecotImage = "dovecot/dovecot:2.3.21"
	dovecotCtr   = "mailbox-e2e-dovecot"
	dovecotUser  = "test"
	dovecotPass  = "testpass"

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

func TestDovecotIntegration(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	addr := startDovecot(t)
	seedMessage(t, addr, testRawMessage)

	cl, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	folders, err := cl.ListMailFolders(ctx)
	if err != nil {
		t.Fatalf("ListMailFolders: %v", err)
	}
	if !hasFolder(folders, "INBOX") {
		t.Fatalf("INBOX not listed: %+v", folders)
	}

	msgs, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Subject != "Hello there" {
		t.Errorf("Subject = %q", msgs[0].Subject)
	}
	if msgs[0].IsRead {
		t.Error("freshly appended message is read")
	}

	full, err := cl.GetMessage(ctx, msgs[0].ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if !strings.Contains(full.Body.Content, "body of the message") {
		t.Errorf("Body = %q", full.Body.Content)
	}

	// Peek validation: a real server sets \Seen on a non-peek BODY[] fetch.
	// After GetMessage the message must still be unread.
	after, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{})
	if err != nil {
		t.Fatalf("ListMessages (post-get): %v", err)
	}
	if len(after) == 1 && after[0].IsRead {
		t.Error("GetMessage marked the message read — BODY[] fetch is missing PEEK")
	}
}

func hasFolder(folders []mail.MailFolder, name string) bool {
	for _, f := range folders {
		if f.DisplayName == name {
			return true
		}
	}
	return false
}

// startDovecot runs a Dovecot container and returns its host IMAP address.
func startDovecot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // readable by the container uid
		t.Fatal(err)
	}
	confPath := dir + "/dovecot.conf"
	if err := os.WriteFile(confPath, []byte(dovecotConf), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", dovecotCtr).Run()

	addr := freeLocalAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	out, err := exec.Command("docker", "run", "-d", "--name", dovecotCtr,
		"-v", confPath+":/etc/dovecot/dovecot.conf:ro",
		"-p", "127.0.0.1:"+port+":143",
		dovecotImage).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run dovecot: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", dovecotCtr).Run() })

	waitIMAP(t, addr)
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
		t.Fatalf("seed append close: %v", err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	_ = c.Logout().Wait()
}

// waitIMAP blocks until the server emits its IMAP greeting.
func waitIMAP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			line, _ := bufio.NewReader(conn).ReadString('\n')
			_ = conn.Close()
			if strings.Contains(line, "OK") {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("dovecot did not become ready in time")
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

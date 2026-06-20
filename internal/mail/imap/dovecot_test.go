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
	appendToINBOX(t, addr, testRawMessage)

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

	msgs, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
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
	after, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages (post-get): %v", err)
	}
	if len(after) == 1 && after[0].IsRead {
		t.Error("GetMessage marked the message read — BODY[] fetch is missing PEEK")
	}

	// Write round-trip against real Dovecot: mark the seeded message read, verify
	// via re-list, then delete it and verify it is gone.
	id := msgs[0].ID
	if err := cl.SetRead(ctx, id, true); err != nil {
		t.Fatalf("SetRead(true): %v", err)
	}
	read, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages (post-setread): %v", err)
	}
	if len(read) != 1 || !read[0].IsRead {
		t.Fatalf("after SetRead(true), got %d messages, IsRead=%v; want 1 read", len(read), len(read) == 1 && read[0].IsRead)
	}

	if err := cl.DeleteMessage(ctx, id); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	gone, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages (post-delete): %v", err)
	}
	if len(gone) != 0 {
		t.Errorf("after DeleteMessage, got %d messages, want 0", len(gone))
	}
}

func TestDovecotRawMessage(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	addr := startDovecot(t)
	appendToINBOX(t, addr, testRawMessage)

	cl, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()

	msgs, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages: %v (n=%d)", err, len(msgs))
	}

	raw, err := cl.RawMessage(ctx, msgs[0].ID)
	if err != nil {
		t.Fatalf("RawMessage: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "Subject: Hello there") {
		t.Errorf("raw message missing Subject header; got %q", got)
	}
	if !strings.Contains(got, "This is the body of the message.") {
		t.Errorf("raw message missing body; got %q", got)
	}

	// Peek validation against a real server: RawMessage must not set \Seen.
	after, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages (post-raw): %v", err)
	}
	if len(after) == 1 && after[0].IsRead {
		t.Error("RawMessage marked the message read — BODY[] fetch is missing PEEK")
	}
}

func TestDovecotDelta(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	addr := startDovecot(t)
	appendToINBOX(t, addr, testRawMessage)

	cl, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()
	ctx := context.Background()
	fid := folderID("INBOX")

	// Initial delta: the seeded message plus a fresh token.
	first, tok, err := cl.Delta(ctx, fid, "")
	if err != nil {
		t.Fatalf("Delta (initial): %v", err)
	}
	if len(first) != 1 || first[0].Subject != "Hello there" {
		t.Fatalf("initial Delta: got %d messages (subj %q), want 1 'Hello there'",
			len(first), subjectOrEmpty(first))
	}
	if tok == "" {
		t.Fatal("initial Delta returned an empty token")
	}

	// A new message arrives, then delta-by-token returns just it.
	newRaw := "From: Carol <carol@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Second message\r\n" +
		"Date: Thu, 12 Jun 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"A new arrival.\r\n"
	appendToINBOX(t, addr, newRaw)

	second, tok2, err := cl.Delta(ctx, fid, tok)
	if err != nil {
		t.Fatalf("Delta (incremental): %v", err)
	}
	if len(second) != 1 || second[0].Subject != "Second message" {
		t.Fatalf("incremental Delta: got %d messages (subj %q), want 1 'Second message'",
			len(second), subjectOrEmpty(second))
	}
	if tok2 == "" || tok2 == tok {
		t.Errorf("incremental Delta token = %q, want an advanced token (was %q)", tok2, tok)
	}

	// CONDSTORE's win over additive sync: a FLAG change (not just a new arrival)
	// advances a message's MODSEQ, so delta-by-token re-reports it with the new
	// state. Mark the seeded message read; the next delta must surface it as read.
	if err := cl.SetRead(ctx, first[0].ID, true); err != nil {
		t.Fatalf("SetRead: %v", err)
	}
	changed, tok3, err := cl.Delta(ctx, fid, tok2)
	if err != nil {
		t.Fatalf("Delta (flag change): %v", err)
	}
	if len(changed) != 1 || changed[0].Subject != "Hello there" || !changed[0].IsRead {
		t.Fatalf("flag-change Delta: got %d messages (subj %q, read=%v), want the now-read 'Hello there'",
			len(changed), subjectOrEmpty(changed), len(changed) == 1 && changed[0].IsRead)
	}
	if tok3 == "" || tok3 == tok2 {
		t.Errorf("flag-change Delta token = %q, want an advanced token (was %q)", tok3, tok2)
	}

	// With the latest token nothing has changed: an empty result and a stable token.
	none, tok4, err := cl.Delta(ctx, fid, tok3)
	if err != nil {
		t.Fatalf("Delta (no change): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("no-change Delta: got %d messages, want 0", len(none))
	}
	if tok4 != tok3 {
		t.Errorf("no-change Delta token = %q, want it unchanged (%q)", tok4, tok3)
	}
}

// TestDovecotIdle exercises the IDLE watcher against a real Dovecot server,
// which supports IDLE natively: a watcher idling on INBOX must see onChange fire
// when a message is appended, and Watch must return nil once ctx is cancelled.
// The append uses a fresh session (appendToINBOX dials its own connection),
// since IDLE monopolizes the watcher's session.
func TestDovecotIdle(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	addr := startDovecot(t)

	watcher, err := Dial(addr, dovecotUser, dovecotPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial (watcher): %v", err)
	}
	defer func() { _ = watcher.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changed := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- watcher.Watch(ctx, folderID("INBOX"), func() { changed <- struct{}{} })
	}()

	// Let the watcher SELECT and enter IDLE before the append so the arrival is
	// delivered live.
	time.Sleep(500 * time.Millisecond)
	appendToINBOX(t, addr, testRawMessage)

	select {
	case <-changed:
		// onChange fired.
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not fire onChange within 10s of an APPEND")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return within 10s of ctx cancel")
	}
}

func subjectOrEmpty(msgs []mail.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[0].Subject
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

// appendToINBOX appends raw to INBOX via a raw IMAP client.
func appendToINBOX(t *testing.T, addr, raw string) {
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

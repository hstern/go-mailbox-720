package imap

import (
	"context"
	"net"
	"strings"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

const (
	testUser = "user"
	testPass = "pass"

	// A simple RFC 822 message (CRLF line endings, as on the wire).
	testRawMessage = "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Hello there\r\n" +
		"Date: Wed, 11 Jun 2025 12:00:00 +0000\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"This is the body of the message.\r\n"
)

// newMemoryIMAP starts an in-process IMAP server with one user whose INBOX
// holds testRawMessage, and returns its address.
func newMemoryIMAP(t *testing.T) string {
	t.Helper()
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPass)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := user.Append("INBOX", strings.NewReader(testRawMessage), &goimap.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	memServer.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })
	return ln.Addr().String()
}

func dialTest(t *testing.T) *Client {
	t.Helper()
	cl, err := Dial(newMemoryIMAP(t), testUser, testPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

func TestListMailFolders(t *testing.T) {
	cl := dialTest(t)
	folders, err := cl.ListMailFolders(context.Background())
	if err != nil {
		t.Fatalf("ListMailFolders: %v", err)
	}
	var inbox *struct{ total, unread int }
	for _, f := range folders {
		if f.DisplayName == "INBOX" {
			inbox = &struct{ total, unread int }{f.Total, f.Unread}
		}
	}
	if inbox == nil {
		t.Fatalf("INBOX not found in %+v", folders)
	}
	if inbox.total != 1 {
		t.Errorf("INBOX Total = %d, want 1", inbox.total)
	}
	if inbox.unread != 1 {
		t.Errorf("INBOX Unread = %d, want 1", inbox.unread)
	}
}

func TestListMessages(t *testing.T) {
	cl := dialTest(t)
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Subject != "Hello there" {
		t.Errorf("Subject = %q, want %q", m.Subject, "Hello there")
	}
	if m.From.Email != "alice@example.com" {
		t.Errorf("From = %q, want alice@example.com", m.From.Email)
	}
	if m.IsRead {
		t.Error("IsRead = true, want false (freshly appended)")
	}
	if m.Body.Content != "" {
		t.Error("ListMessages should not populate Body")
	}
}

func TestGetMessage(t *testing.T) {
	cl := dialTest(t)
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages: %v (n=%d)", err, len(msgs))
	}
	full, err := cl.GetMessage(context.Background(), msgs[0].ID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if full.Subject != "Hello there" {
		t.Errorf("Subject = %q", full.Subject)
	}
	if !strings.Contains(full.Body.Content, "body of the message") {
		t.Errorf("Body = %q, want it to contain the message body", full.Body.Content)
	}
	if full.Body.ContentType != "text" {
		t.Errorf("Body.ContentType = %q, want text", full.Body.ContentType)
	}
}

func TestParseBodyPrefersHTML(t *testing.T) {
	raw := "Subject: x\r\n" +
		"Content-Type: multipart/alternative; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nplain version\r\n" +
		"--B\r\nContent-Type: text/html\r\n\r\n<p>html version</p>\r\n" +
		"--B--\r\n"
	body, prev, hasAttach := parseBody([]byte(raw))
	if body.ContentType != "html" || !strings.Contains(body.Content, "html version") {
		t.Errorf("body = %+v, want html with 'html version'", body)
	}
	if hasAttach {
		t.Error("hasAttach = true, want false")
	}
	if prev == "" {
		t.Error("preview is empty")
	}
}

func TestParseBodyDetectsAttachment(t *testing.T) {
	raw := "Subject: x\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"f.bin\"\r\n\r\nDATA\r\n" +
		"--B--\r\n"
	body, _, hasAttach := parseBody([]byte(raw))
	if !hasAttach {
		t.Error("hasAttach = false, want true")
	}
	if !strings.Contains(body.Content, "see attached") {
		t.Errorf("body = %q, want it to contain the text part", body.Content)
	}
}

func TestGetMessageStaleID(t *testing.T) {
	cl := dialTest(t)
	// Same mailbox + uid but a wrong UIDVALIDITY must be rejected, not mis-served.
	stale := messageID("INBOX", 999999, 1)
	if _, err := cl.GetMessage(context.Background(), stale); err == nil {
		t.Error("GetMessage with stale UIDVALIDITY = nil error, want error")
	}
}

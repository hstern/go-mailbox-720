package imap

import (
	"context"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
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
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{}, nil)
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
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{}, nil)
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

func TestSetRead(t *testing.T) {
	cl := dialTest(t)
	ctx := context.Background()

	msgs, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages: %v (n=%d)", err, len(msgs))
	}
	id := msgs[0].ID
	if msgs[0].IsRead {
		t.Fatal("freshly appended message is read")
	}

	// Mark read, then confirm both re-list and GetMessage report IsRead true.
	if err := cl.SetRead(ctx, id, true); err != nil {
		t.Fatalf("SetRead(true): %v", err)
	}
	after, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil || len(after) != 1 {
		t.Fatalf("ListMessages (post-set): %v (n=%d)", err, len(after))
	}
	if !after[0].IsRead {
		t.Error("after SetRead(true), IsRead = false, want true")
	}
	full, err := cl.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if !full.IsRead {
		t.Error("after SetRead(true), GetMessage IsRead = false, want true")
	}

	// Clear it again.
	if err := cl.SetRead(ctx, id, false); err != nil {
		t.Fatalf("SetRead(false): %v", err)
	}
	cleared, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil || len(cleared) != 1 {
		t.Fatalf("ListMessages (post-clear): %v (n=%d)", err, len(cleared))
	}
	if cleared[0].IsRead {
		t.Error("after SetRead(false), IsRead = true, want false")
	}
}

func TestDeleteMessage(t *testing.T) {
	cl := dialTest(t)
	ctx := context.Background()

	msgs, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages: %v (n=%d)", err, len(msgs))
	}

	if err := cl.DeleteMessage(ctx, msgs[0].ID); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	after, err := cl.ListMessages(ctx, folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages (post-delete): %v", err)
	}
	if len(after) != 0 {
		t.Errorf("after DeleteMessage, got %d messages, want 0", len(after))
	}
}

func TestWriteStaleID(t *testing.T) {
	cl := dialTest(t)
	ctx := context.Background()
	stale := messageID("INBOX", 999999, 1)
	if err := cl.SetRead(ctx, stale, true); err == nil {
		t.Error("SetRead with stale UIDVALIDITY = nil error, want error")
	}
	if err := cl.DeleteMessage(ctx, stale); err == nil {
		t.Error("DeleteMessage with stale UIDVALIDITY = nil error, want error")
	}
}

// seedMessage describes one message to append to a fresh INBOX for filter tests.
type seedMessage struct {
	subject  string
	from     string
	to       string
	received time.Time
	read     bool
}

// raw renders the seed as an RFC 822 message.
func (s seedMessage) raw() string {
	return "From: " + s.from + "\r\n" +
		"To: " + s.to + "\r\n" +
		"Subject: " + s.subject + "\r\n" +
		"Date: " + s.received.Format(time.RFC1123Z) + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body\r\n"
}

// dialFiltered starts a fresh in-process IMAP server seeded with the given
// messages and returns a connected Client. Each message's INTERNALDATE is set to
// its received time and its \Seen flag to its read state, so receivedDateTime and
// isRead filters have something to act on.
func dialFiltered(t *testing.T, seeds ...seedMessage) *Client {
	t.Helper()
	memServer := imapmemserver.New()
	user := imapmemserver.NewUser(testUser, testPass)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range seeds {
		opts := &goimap.AppendOptions{Time: s.received}
		if s.read {
			opts.Flags = []goimap.Flag{goimap.FlagSeen}
		}
		if _, err := user.Append("INBOX", strings.NewReader(s.raw()), opts); err != nil {
			t.Fatal(err)
		}
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

	cl, err := Dial(ln.Addr().String(), testUser, testPass, &Options{TLS: false})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// subjectsOf returns the message subjects, sorted, for set-wise comparison.
func subjectsOf(msgs []mail.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

func mustParse(t *testing.T, s string) *odata.Filter {
	t.Helper()
	f, err := odata.Parse(s)
	if err != nil {
		t.Fatalf("odata.Parse(%q): %v", s, err)
	}
	return f
}

func TestListMessagesFilter(t *testing.T) {
	t1 := time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 6, 10, 9, 0, 0, 0, time.UTC)
	t3 := time.Date(2025, 6, 20, 9, 0, 0, 0, time.UTC)
	seeds := []seedMessage{
		{subject: "Project update", from: "Alice <alice@example.com>", to: "Bob <bob@example.com>", received: t1, read: true},
		{subject: "Lunch plans", from: "Carol <carol@other.com>", to: "Bob <bob@example.com>", received: t2, read: false},
		{subject: "Project deadline", from: "Alice <alice@example.com>", to: "Dave <dave@example.com>", received: t3, read: false},
	}

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{
			name:   "subject contains",
			filter: "contains(subject,'Project')",
			want:   []string{"Project deadline", "Project update"},
		},
		{
			name:   "subject startswith",
			filter: "startswith(subject,'Lunch')",
			want:   []string{"Lunch plans"},
		},
		{
			name:   "subject endswith (client-side only)",
			filter: "endswith(subject,'update')",
			want:   []string{"Project update"},
		},
		{
			name:   "from equals via nested path",
			filter: "from/emailAddress/address eq 'alice@example.com'",
			want:   []string{"Project deadline", "Project update"},
		},
		{
			name:   "isRead true",
			filter: "isRead eq true",
			want:   []string{"Project update"},
		},
		{
			name:   "isRead false",
			filter: "isRead eq false",
			want:   []string{"Lunch plans", "Project deadline"},
		},
		{
			name:   "received on or after",
			filter: "receivedDateTime ge 2025-06-10T00:00:00Z",
			want:   []string{"Lunch plans", "Project deadline"},
		},
		{
			name:   "received before",
			filter: "receivedDateTime lt 2025-06-10T00:00:00Z",
			want:   []string{"Project update"},
		},
		{
			// le with a non-midnight bound must still include same-day messages
			// (t2 is 2025-06-10 09:00): IMAP BEFORE is date-only, so the candidate
			// window must cover all of that day. Regression for the SEARCH-superset
			// bug where BEFORE 10-Jun excluded the whole 10th.
			name:   "received on or before non-midnight bound",
			filter: "receivedDateTime le 2025-06-10T10:00:00Z",
			want:   []string{"Lunch plans", "Project update"},
		},
		{
			name:   "and combines predicates",
			filter: "contains(subject,'Project') and isRead eq false",
			want:   []string{"Project deadline"},
		},
		{
			name:   "or combines predicates",
			filter: "startswith(subject,'Lunch') or isRead eq true",
			want:   []string{"Lunch plans", "Project update"},
		},
		{
			name:   "not negates",
			filter: "not contains(subject,'Project')",
			want:   []string{"Lunch plans"},
		},
		{
			name:   "no match",
			filter: "contains(subject,'Invoice')",
			want:   []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := dialFiltered(t, seeds...)
			msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{}, mustParse(t, tc.filter))
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			got := subjectsOf(msgs)
			if !equalStrings(got, tc.want) {
				t.Errorf("subjects = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListMessagesFilterNilIsUnfiltered(t *testing.T) {
	seeds := []seedMessage{
		{subject: "one", from: "a@x.com", to: "b@x.com", received: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
		{subject: "two", from: "a@x.com", to: "b@x.com", received: time.Date(2025, 6, 2, 0, 0, 0, 0, time.UTC)},
	}
	cl := dialFiltered(t, seeds...)
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{}, nil)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (nil filter must not filter)", len(msgs))
	}
}

func TestListMessagesFilterPaging(t *testing.T) {
	var seeds []seedMessage
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seeds = append(seeds, seedMessage{
			subject:  "Project " + string(rune('A'+i)),
			from:     "a@x.com",
			to:       "b@x.com",
			received: base.AddDate(0, 0, i),
		})
	}
	cl := dialFiltered(t, seeds...)
	// All five match; Top=2 newest-first should yield the two latest received.
	msgs, err := cl.ListMessages(context.Background(), folderID("INBOX"), mail.Page{Top: 2}, mustParse(t, "contains(subject,'Project')"))
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Subject != "Project E" || msgs[1].Subject != "Project D" {
		t.Errorf("got %q,%q; want newest-first Project E, Project D", msgs[0].Subject, msgs[1].Subject)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

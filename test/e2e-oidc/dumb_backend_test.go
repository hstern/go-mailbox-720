// This file is the MB720-15 unified dumb-backend-tier e2e: it stands up the whole
// stack — Kanidm (OIDC IdP), Dovecot (IMAP mail), Radicale (CalDAV + CardDAV), an
// in-process SMTP sink, and the real mailboxd wired to all of them with the
// client-side iTIP/iMIP scheduling engine ON — and drives the core Graph mailbox
// flows over HTTP against the real services.
//
// Because this tier has no RFC 6638 server-side scheduling (Radicale is dumb
// storage), the client-side iTIP/iMIP engine is the cooperation under test:
//
//   - Outbound (organizer side): a Graph client POSTs /me/events with an attendee;
//     mailboxd emails a METHOD:REQUEST iMIP invitation, captured by the SMTP sink.
//   - Inbound (attendee side): a METHOD:REQUEST iMIP email is delivered to the
//     Dovecot inbox; the scheduling trigger turns it into a tentative calendar
//     event in Radicale, read back via GET /me/events.
//
// It reuses the harness from oidc_test.go (Kanidm/Dovecot/Radicale launchers,
// token minting, HTTP helpers) and adds the CardDAV, SMTP, and scheduling wiring
// the single-vertical-slice TestOIDCEndToEnd does not exercise. The test
// self-skips when docker is unavailable.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message/mail"
)

// mailboxOwner is the mailbox's own address: the organizer on outbound invites
// and the attendee on inbound ones. It is the address Kanidm-authenticated
// requests act as (single-mailbox model).
const mailboxOwner = "bob@example.com"

// inviteeAddress is an external attendee added to an outbound event so mailboxd
// has someone to send the METHOD:REQUEST to.
const inviteeAddress = "dana@example.com"

func TestDumbBackendEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil { // readable by the container's uid
		t.Fatal(err)
	}
	caPool := writeCerts(t, dir)
	writeServerConfig(t, dir)

	// --- Real services: IdP, mail, calendar+contacts, mail sink. ---
	startKanidm(t, dir, caPool)
	adminPassword := recoverAdmin(t)
	secret := provision(t, dir, adminPassword)

	dovecotAddr := startDovecot(t)
	seedMessage(t, dovecotAddr, testMessage)

	radicaleBase := startRadicale(t)
	seedCalendar(t, radicaleBase)
	seedAddressBook(t, radicaleBase)

	sink := startSMTPSink(t)

	base := startDumbBackendMailboxd(t, dir, secret, dovecotAddr, radicaleBase, sink.addr)
	token := mintToken(t, caClient(caPool), secret)

	// --- Auth: unauthenticated is rejected, authenticated is accepted. ---
	if got := status(t, base+"/me/messages", ""); got != http.StatusUnauthorized {
		t.Errorf("unauthenticated /me/messages: status = %d, want 401", got)
	}

	t.Run("Mail", func(t *testing.T) { testMail(t, base, token) })
	t.Run("Calendar", func(t *testing.T) { testCalendar(t, base, token) })
	t.Run("Contacts", func(t *testing.T) { testContacts(t, base, token) })
	t.Run("OutboundInvite", func(t *testing.T) { testOutboundInvite(t, base, token, sink) })
	t.Run("InboundInvite", func(t *testing.T) { testInboundInvite(t, base, token, dovecotAddr) })
}

// testMail exercises the message/folder read path and a PATCH (mark read) against
// the real Dovecot inbox.
func testMail(t *testing.T, base, token string) {
	// Messages: the seeded message comes back as Graph JSON.
	code, body := get(t, base+"/me/messages", token)
	if code != http.StatusOK {
		t.Fatalf("/me/messages: status = %d, body = %s", code, body)
	}
	var msgs struct {
		Value []struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			IsRead  bool   `json:"isRead"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &msgs); err != nil {
		t.Fatalf("decode messages: %v (%s)", err, body)
	}
	if len(msgs.Value) != 1 {
		t.Fatalf("got %d messages, want 1: %s", len(msgs.Value), body)
	}
	if got := msgs.Value[0].Subject; got != "Hello there" {
		t.Errorf("subject = %q, want %q", got, "Hello there")
	}

	// Folders: the mailbox lists its IMAP folders, including the INBOX.
	fcode, fbody := get(t, base+"/me/mailFolders", token)
	if fcode != http.StatusOK {
		t.Fatalf("/me/mailFolders: status = %d, body = %s", fcode, fbody)
	}
	var folders struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(fbody, &folders); err != nil {
		t.Fatalf("decode folders: %v (%s)", err, fbody)
	}
	if !hasFolder(folders.Value, "Inbox") && !hasFolder(folders.Value, "INBOX") {
		t.Errorf("mailFolders missing an Inbox: %s", fbody)
	}

	// PATCH: mark the message read, then confirm the change round-trips. Graph
	// request bodies carry an @odata.type on every object (the subsetted schema
	// requires it), so it is set throughout the write payloads below.
	id := msgs.Value[0].ID
	pcode, pbody := doJSON(t, http.MethodPatch, base+"/me/messages/"+id, token,
		`{"@odata.type":"#microsoft.graph.message","isRead":true}`)
	if pcode != http.StatusOK {
		t.Fatalf("PATCH /me/messages: status = %d, body = %s", pcode, pbody)
	}
	gcode, gbody := get(t, base+"/me/messages/"+id, token)
	if gcode != http.StatusOK {
		t.Fatalf("GET message after PATCH: status = %d, body = %s", gcode, gbody)
	}
	var got struct {
		IsRead bool `json:"isRead"`
	}
	if err := json.Unmarshal(gbody, &got); err != nil {
		t.Fatalf("decode patched message: %v (%s)", err, gbody)
	}
	if !got.IsRead {
		t.Errorf("isRead = false after PATCH isRead:true")
	}

	// Delta: the messages delta endpoint returns a deltaLink for the next round.
	dcode, dbody := get(t, base+"/me/messages/delta()", token)
	if dcode != http.StatusOK {
		t.Fatalf("/me/messages/delta(): status = %d, body = %s", dcode, dbody)
	}
	assertDeltaLink(t, "messages", dbody)
}

// testCalendar exercises the event read path, a create (without attendees, so no
// scheduling), and the events delta.
func testCalendar(t *testing.T, base, token string) {
	// The seeded event comes back.
	code, body := get(t, base+"/me/events", token)
	if code != http.StatusOK {
		t.Fatalf("/me/events: status = %d, body = %s", code, body)
	}
	var events struct {
		Value []struct {
			Subject string `json:"subject"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode events: %v (%s)", err, body)
	}
	if !hasEvent(events.Value, eventSummary) {
		t.Errorf("events missing %q: %s", eventSummary, body)
	}

	// Create an event with no attendees: it is stored but triggers no iMIP send.
	create := `{"@odata.type":"#microsoft.graph.event","subject":"Solo Focus Time",` +
		`"start":{"@odata.type":"#microsoft.graph.dateTimeTimeZone","dateTime":"2026-07-01T09:00:00","timeZone":"UTC"},` +
		`"end":{"@odata.type":"#microsoft.graph.dateTimeTimeZone","dateTime":"2026-07-01T10:00:00","timeZone":"UTC"}}`
	ccode, cbody := doJSON(t, http.MethodPost, base+"/me/events", token, create)
	if ccode != http.StatusCreated {
		t.Fatalf("POST /me/events: status = %d, body = %s", ccode, cbody)
	}

	// The new event is now listed.
	_, lbody := get(t, base+"/me/events", token)
	var after struct {
		Value []struct {
			Subject string `json:"subject"`
		} `json:"value"`
	}
	if err := json.Unmarshal(lbody, &after); err != nil {
		t.Fatalf("decode events after create: %v (%s)", err, lbody)
	}
	if !hasEvent(after.Value, "Solo Focus Time") {
		t.Errorf("created event not listed: %s", lbody)
	}

	// Delta: the events delta endpoint returns a deltaLink.
	dcode, dbody := get(t, base+"/me/events/delta()", token)
	if dcode != http.StatusOK {
		t.Fatalf("/me/events/delta(): status = %d, body = %s", dcode, dbody)
	}
	assertDeltaLink(t, "events", dbody)
}

// testContacts exercises the contact read path, a create, and the contacts delta
// against the real Radicale CardDAV collection.
func testContacts(t *testing.T, base, token string) {
	// The seeded contact comes back.
	code, body := get(t, base+"/me/contacts", token)
	if code != http.StatusOK {
		t.Fatalf("/me/contacts: status = %d, body = %s", code, body)
	}
	var contacts struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &contacts); err != nil {
		t.Fatalf("decode contacts: %v (%s)", err, body)
	}
	if !hasContact(contacts.Value, contactFullName) {
		t.Errorf("contacts missing %q: %s", contactFullName, body)
	}

	// Create a contact, then confirm it lists.
	create := `{"@odata.type":"#microsoft.graph.contact",` +
		`"givenName":"Erin","surname":"Example","displayName":"Erin Example",` +
		`"emailAddresses":[{"@odata.type":"#microsoft.graph.emailAddress","address":"erin@example.com","name":"Erin Example"}]}`
	ccode, cbody := doJSON(t, http.MethodPost, base+"/me/contacts", token, create)
	if ccode != http.StatusCreated {
		t.Fatalf("POST /me/contacts: status = %d, body = %s", ccode, cbody)
	}
	_, lbody := get(t, base+"/me/contacts", token)
	var after struct {
		Value []struct {
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.Unmarshal(lbody, &after); err != nil {
		t.Fatalf("decode contacts after create: %v (%s)", err, lbody)
	}
	if !hasContact(after.Value, "Erin Example") {
		t.Errorf("created contact not listed: %s", lbody)
	}

	// Delta: the contacts delta endpoint returns a deltaLink.
	dcode, dbody := get(t, base+"/me/contacts/delta()", token)
	if dcode != http.StatusOK {
		t.Fatalf("/me/contacts/delta(): status = %d, body = %s", dcode, dbody)
	}
	assertDeltaLink(t, "contacts", dbody)
}

// testOutboundInvite is the organizer side of the iTIP cooperation: POSTing an
// event with an attendee makes mailboxd email a METHOD:REQUEST iMIP invitation,
// which the in-process SMTP sink captures. This is the path the dumb-backend tier
// must drive itself, since Radicale does no server-side scheduling.
func testOutboundInvite(t *testing.T, base, token string, sink *smtpSink) {
	before := len(sink.messages())

	create := fmt.Sprintf(`{"@odata.type":"#microsoft.graph.event","subject":"Project Kickoff",`+
		`"start":{"@odata.type":"#microsoft.graph.dateTimeTimeZone","dateTime":"2026-07-02T15:00:00","timeZone":"UTC"},`+
		`"end":{"@odata.type":"#microsoft.graph.dateTimeTimeZone","dateTime":"2026-07-02T16:00:00","timeZone":"UTC"},`+
		`"organizer":{"@odata.type":"#microsoft.graph.recipient","emailAddress":{"@odata.type":"#microsoft.graph.emailAddress","address":%q}},`+
		`"attendees":[{"@odata.type":"#microsoft.graph.attendee","type":"required","emailAddress":{"@odata.type":"#microsoft.graph.emailAddress","address":%q,"name":"Dana"}}]}`,
		mailboxOwner, inviteeAddress)
	ccode, cbody := doJSON(t, http.MethodPost, base+"/me/events", token, create)
	if ccode != http.StatusCreated {
		t.Fatalf("POST /me/events (with attendee): status = %d, body = %s", ccode, cbody)
	}

	// The send is best-effort and off the request path, so poll the sink.
	msg := waitForMail(t, sink, before, 20*time.Second, func(m capturedMail) bool {
		return containsRcpt(m.to, inviteeAddress) && isMethodRequest(m.data)
	})
	if msg == nil {
		t.Fatalf("no METHOD:REQUEST iMIP invitation captured for %s", inviteeAddress)
	}
	if !bytes.Contains(msg.data, []byte("Project Kickoff")) {
		t.Errorf("captured invitation missing the event subject:\n%s", msg.data)
	}
}

// testInboundInvite is the attendee side of the iTIP cooperation: delivering a
// METHOD:REQUEST iMIP email to the Dovecot inbox makes the scheduling trigger
// create a tentative event in Radicale, read back via GET /me/events.
func testInboundInvite(t *testing.T, base, token, dovecotAddr string) {
	const inboundSubject = "Inbound Sync Review"
	seedMessage(t, dovecotAddr, inviteRequestMail(t, inboundSubject))

	// The trigger runs off an IMAP IDLE watch; the tentative event appears
	// asynchronously, so poll /me/events for it.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, body := get(t, base+"/me/events", token)
		var events struct {
			Value []struct {
				Subject string `json:"subject"`
			} `json:"value"`
		}
		if err := json.Unmarshal(body, &events); err == nil && hasEvent(events.Value, inboundSubject) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("inbound REQUEST never became a tentative event %q", inboundSubject)
}

// startDumbBackendMailboxd builds and runs mailboxd wired to the full dumb-backend
// stack: Kanidm auth, Dovecot IMAP, Radicale CalDAV + CardDAV, the SMTP sink, and
// the client-side scheduling engine ON. It returns the /v1.0 base URL.
func startDumbBackendMailboxd(t *testing.T, dir, secret, imapAddr, davURL, smtpAddr string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mailboxd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/mailboxd")
	build.Dir = "../.."
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build mailboxd (did you run `go generate ./internal/graph`?): %v", err)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin,
		"-addr", addr,
		"-auth-issuer", kanidmBase+"/oauth2/openid/"+rsClientID,
		"-auth-audience", rsClientID,
		"-auth-scope", rsScope,
		"-auth-introspect-client-id", rsClientID,
		"-mail-imap-addr", imapAddr,
		"-mail-imap-username", dovecotUser,
		"-mail-imap-tls=false",
		"-cal-caldav-url", davURL,
		"-cal-caldav-username", radicaleUser,
		"-contacts-carddav-url", davURL,
		"-contacts-carddav-username", radicaleUser,
		"-smtp-addr", smtpAddr,
		"-smtp-tls=false",
		"-smtp-starttls=false",
		"-mailbox-email", mailboxOwner,
		"-enable-scheduling",
	)
	cmd.Env = append(os.Environ(),
		"SSL_CERT_FILE="+filepath.Join(dir, "ca.pem"), // trust Kanidm's CA for discovery/introspection
		"MAILBOXD_INTROSPECT_CLIENT_SECRET="+secret,
		"MAILBOXD_IMAP_PASSWORD="+dovecotPass,
		"MAILBOXD_CALDAV_PASSWORD="+radicalePass,
		"MAILBOXD_CARDDAV_PASSWORD="+radicalePass,
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

// --- HTTP + iMIP helpers ---

// doJSON issues method url with a JSON body and bearer token, returning the
// status and response body.
func doJSON(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// assertDeltaLink asserts a delta page carries an @odata.deltaLink (the token a
// client GETs for the next sync round).
func assertDeltaLink(t *testing.T, what string, body []byte) {
	t.Helper()
	var page struct {
		DeltaLink string `json:"@odata.deltaLink"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode %s delta page: %v (%s)", what, err, body)
	}
	if page.DeltaLink == "" {
		t.Errorf("%s delta page missing @odata.deltaLink: %s", what, body)
	}
}

// waitForMail polls the sink for a message past index `since` matching pred,
// returning it or nil on timeout.
func waitForMail(t *testing.T, sink *smtpSink, since int, timeout time.Duration, pred func(capturedMail) bool) *capturedMail {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msgs := sink.messages()
		for i := since; i < len(msgs); i++ {
			if pred(msgs[i]) {
				m := msgs[i]
				return &m
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

func containsRcpt(to []string, addr string) bool {
	for _, r := range to {
		if strings.EqualFold(strings.Trim(r, "<>"), addr) {
			return true
		}
	}
	return false
}

// isMethodRequest reports whether raw is an iMIP message carrying a
// METHOD:REQUEST iCalendar part (the organizer's invitation).
func isMethodRequest(raw []byte) bool {
	return bytes.Contains(raw, []byte("METHOD:REQUEST"))
}

// inviteRequestMail builds an iMIP METHOD:REQUEST email addressed to the mailbox
// owner: a multipart/alternative message with a text/calendar part (method=REQUEST)
// carrying a VEVENT the inbound scheduling trigger turns into a tentative event.
//
// It is composed with emersion/go-message/mail (the same library mailboxd's iMIP
// engine parses with) so the part structure is exactly what the scheduling trigger
// expects — a hand-rolled MIME body is fragile to parse.
func inviteRequestMail(t *testing.T, subject string) string {
	t.Helper()
	ics := crlf(
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//go-mailbox-720//inbound-e2e//EN",
		"METHOD:REQUEST",
		"BEGIN:VEVENT",
		"UID:inbound-1@go-mailbox-720.test",
		"DTSTAMP:20260101T000000Z",
		"DTSTART:20260720T140000Z",
		"DTEND:20260720T150000Z",
		"ORGANIZER:mailto:"+inviteeAddress,
		"ATTENDEE;RSVP=TRUE:mailto:"+mailboxOwner,
		"SUMMARY:"+subject,
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	)

	var h mail.Header
	h.SetAddressList("From", []*mail.Address{{Name: "Dana", Address: inviteeAddress}})
	h.SetAddressList("To", []*mail.Address{{Name: "Bob", Address: mailboxOwner}})
	h.SetSubject(subject)
	h.SetDate(time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	h.Set("MIME-Version", "1.0")

	var buf bytes.Buffer
	iw, err := mail.CreateInlineWriter(&buf, h)
	if err != nil {
		t.Fatalf("compose invite: create writer: %v", err)
	}
	var ch mail.InlineHeader
	ch.Set("Content-Type", `text/calendar; method=REQUEST; charset="UTF-8"`)
	pw, err := iw.CreatePart(ch)
	if err != nil {
		t.Fatalf("compose invite: create part: %v", err)
	}
	if _, err := io.WriteString(pw, ics); err != nil {
		t.Fatalf("compose invite: write part: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("compose invite: close part: %v", err)
	}
	if err := iw.Close(); err != nil {
		t.Fatalf("compose invite: close writer: %v", err)
	}
	return buf.String()
}

func hasFolder(folders []struct {
	DisplayName string `json:"displayName"`
}, name string) bool {
	for _, f := range folders {
		if strings.EqualFold(f.DisplayName, name) {
			return true
		}
	}
	return false
}

func hasEvent(events []struct {
	Subject string `json:"subject"`
}, subject string) bool {
	for _, e := range events {
		if e.Subject == subject {
			return true
		}
	}
	return false
}

func hasContact(contacts []struct {
	DisplayName string `json:"displayName"`
}, name string) bool {
	for _, c := range contacts {
		if c.DisplayName == name {
			return true
		}
	}
	return false
}

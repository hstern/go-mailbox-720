package scheduling

// This file is the iMIP composition bridge: it wraps an iTIP scheduling object
// (the VCALENDAR bytes produced by Reply/Request/Cancel) in the RFC 822 email
// that carries it to the SMTP send port.
//
// iMIP (RFC 6047) defines how an iTIP method travels over email. The message is
// an ordinary Internet Message (RFC 5322) whose calendar payload lives in a
// "text/calendar" body part bearing a "method" Content-Type parameter equal to
// the iTIP METHOD (e.g. `Content-Type: text/calendar; method=REPLY;
// charset=UTF-8`). A human-readable text/plain part is customary, so the two
// are wrapped in a multipart/alternative: a mail client that does not grok
// calendaring still shows the user something, while one that does acts on the
// calendar part. The composed message round-trips through Parse — its
// text/calendar part and method parameter are the same shape Parse looks for.

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-message/mail"
)

// Compose builds the RFC 822 / RFC 6047 iMIP message carrying an iTIP
// scheduling object. The body is a multipart/alternative with two inline parts:
// a short human-readable text/plain summary and a "text/calendar" part holding
// ics with the method=<method> and charset=UTF-8 Content-Type parameters. The
// From/To/Subject/Date/MIME-Version headers are set from the arguments, and a
// deterministic Message-ID is derived from the ics UID (no time.Now, no
// randomness, so callers and tests stay reproducible).
//
// The serialized bytes are ready for the SMTP Sender's Send(ctx, from, to,
// raw): CRLF line endings, headers then a blank line then the body. It returns
// an error if the message cannot be written.
func Compose(from Address, to []Address, subject string, method Method, ics []byte, date time.Time) ([]byte, error) {
	if from.Email == "" {
		return nil, fmt.Errorf("scheduling: compose: no from address")
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("scheduling: compose: no recipients")
	}

	var h mail.Header
	h.SetAddressList("From", []*mail.Address{toMailAddress(from)})
	h.SetAddressList("To", toMailAddresses(to))
	h.SetSubject(subject)
	h.SetDate(date)
	// MIME-Version is written by the writer, but set it explicitly so the header
	// is present even if the underlying library's behavior changes.
	h.Set("MIME-Version", "1.0")
	h.SetMessageID(messageID(ics, from.Email))

	var buf bytes.Buffer
	// CreateInlineWriter emits a multipart/alternative of inline parts — the
	// customary iMIP shape (text/plain alongside text/calendar). The header
	// written here is the top-level message header.
	iw, err := mail.CreateInlineWriter(&buf, h)
	if err != nil {
		return nil, fmt.Errorf("scheduling: compose: create writer: %w", err)
	}

	if err := writePart(iw, "text/plain", map[string]string{"charset": "UTF-8"}, []byte(plainSummary(method, subject))); err != nil {
		return nil, err
	}
	// The calendar part carries the iTIP method as the "method" Content-Type
	// parameter (RFC 6047 §2.4); UTF-8 names the charset of the VCALENDAR text.
	calParams := map[string]string{"method": string(method), "charset": "UTF-8"}
	if err := writePart(iw, "text/calendar", calParams, ics); err != nil {
		return nil, err
	}

	if err := iw.Close(); err != nil {
		return nil, fmt.Errorf("scheduling: compose: close writer: %w", err)
	}
	return buf.Bytes(), nil
}

// ComposeReply is a convenience that builds the REPLY VCALENDAR for the
// responding attendee (via Reply) and wraps it in an iMIP message addressed
// from the attendee to the request's organizer, with a subject reflecting the
// participation status (e.g. "Accepted: Project sync"). It is the one-call path
// from a parsed REQUEST to mailable REPLY bytes.
//
// date is taken as a parameter (no time.Now) for deterministic output. It
// returns an error if req is nil, carries no organizer to reply to, or the
// underlying Reply/Compose fails.
func ComposeReply(req *Invite, attendee Address, partStat PartStat, date time.Time) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("scheduling: compose reply: nil request")
	}
	if req.Organizer.Email == "" {
		return nil, fmt.Errorf("scheduling: compose reply: request has no organizer")
	}
	ics, err := Reply(req, attendee, partStat)
	if err != nil {
		return nil, err
	}
	subject := replySubject(partStat, req.Summary)
	return Compose(attendee, []Address{req.Organizer}, subject, MethodReply, ics, date)
}

// writePart writes one inline part of the multipart/alternative with the given
// media type, Content-Type parameters, and body bytes.
func writePart(iw *mail.InlineWriter, mediaType string, params map[string]string, body []byte) error {
	var ph mail.InlineHeader
	ph.SetContentType(mediaType, params)
	pw, err := iw.CreatePart(ph)
	if err != nil {
		return fmt.Errorf("scheduling: compose: create %s part: %w", mediaType, err)
	}
	if _, err := pw.Write(body); err != nil {
		_ = pw.Close()
		return fmt.Errorf("scheduling: compose: write %s part: %w", mediaType, err)
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("scheduling: compose: close %s part: %w", mediaType, err)
	}
	return nil
}

// plainSummary is the short human-readable text/plain alternative shown by mail
// clients that do not process calendaring. It names the method and the event.
func plainSummary(method Method, subject string) string {
	if subject == "" {
		return fmt.Sprintf("Calendar %s (iMIP/iTIP).\r\n", method)
	}
	return fmt.Sprintf("Calendar %s: %s\r\n", method, subject)
}

// replySubject builds a REPLY subject reflecting the attendee's decision, e.g.
// "Accepted: Project sync".
func replySubject(partStat PartStat, summary string) string {
	verb := "Response"
	switch partStat {
	case PartStatAccepted:
		verb = "Accepted"
	case PartStatDeclined:
		verb = "Declined"
	case PartStatTentative:
		verb = "Tentative"
	}
	if summary == "" {
		return verb
	}
	return verb + ": " + summary
}

// messageID derives a deterministic RFC 5322 Message-ID addr-spec (without the
// surrounding angle brackets — mail.Header.SetMessageID adds those) from the
// scheduling object's UID (read out of the ics) and the sender's domain, so the
// same inputs always yield the same id — no randomness, no clock. It falls back
// to a hash of the ics when no UID is present.
func messageID(ics []byte, fromEmail string) string {
	uid := strings.TrimSpace(uidFromICS(ics))
	// A UID is commonly already a local@domain addr-spec (RFC 5545 recommends
	// the RFC 822 form). When it is, reuse it verbatim — that is both a valid
	// Message-ID and stable correlation back to the scheduling object.
	if at := strings.IndexByte(uid, '@'); at > 0 && at < len(uid)-1 &&
		strings.IndexByte(uid[at+1:], '@') < 0 && noMsgIDDelims(uid) {
		return uid
	}

	domain := "localhost"
	if i := strings.IndexByte(fromEmail, '@'); i >= 0 && i+1 < len(fromEmail) {
		domain = fromEmail[i+1:]
	}
	local := sanitizeMessageIDLocal(uid)
	if local == "" {
		local = fmt.Sprintf("%x", sumICS(ics))
	}
	return fmt.Sprintf("%s@%s", local, domain)
}

// noMsgIDDelims reports whether s is free of characters that would make a
// Message-ID malformed (whitespace and angle brackets).
func noMsgIDDelims(s string) bool {
	return !strings.ContainsAny(s, " \t\r\n<>")
}

// uidFromICS extracts the first VEVENT UID value from raw iCalendar bytes by a
// line scan (cheap, dependency-free, and enough for an id seed). It unfolds
// nothing and trims CR; a UID long enough to fold is rare and a truncated seed
// is still deterministic.
func uidFromICS(ics []byte) string {
	for _, raw := range bytes.Split(ics, []byte("\n")) {
		line := bytes.TrimRight(raw, "\r")
		const prefix = "UID:"
		if len(line) > len(prefix) && bytes.EqualFold(line[:len(prefix)], []byte(prefix)) {
			return string(bytes.TrimSpace(line[len(prefix):]))
		}
	}
	return ""
}

// sanitizeMessageIDLocal strips characters that would make the Message-ID's
// left-hand side malformed (whitespace, angle brackets, and "@" which would
// otherwise duplicate the domain separator).
func sanitizeMessageIDLocal(s string) string {
	return string(bytes.Map(func(r rune) rune {
		switch r {
		case '@', '<', '>', ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, []byte(s)))
}

// sumICS is a small deterministic checksum (FNV-1a, inlined to avoid a new
// import) used only as a Message-ID seed when the ics carries no UID.
func sumICS(ics []byte) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, b := range ics {
		h ^= uint64(b)
		h *= prime
	}
	return h
}

// toMailAddress maps an Address onto go-message's mail.Address.
func toMailAddress(a Address) *mail.Address {
	return &mail.Address{Name: a.Name, Address: a.Email}
}

// toMailAddresses maps a slice of Address onto go-message's mail.Address slice.
func toMailAddresses(as []Address) []*mail.Address {
	out := make([]*mail.Address, 0, len(as))
	for _, a := range as {
		out = append(out, toMailAddress(a))
	}
	return out
}

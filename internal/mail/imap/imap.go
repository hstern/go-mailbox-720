// Package imap implements the mail.Backend port against an IMAP server using
// emersion/go-imap (v2) for the protocol and emersion/go-message for MIME
// parsing. A Client is bound to one authenticated IMAP session.
//
// First cut: the read paths (folders, message listing, single-message fetch with
// body). Deferred to their own issues: IDLE→push (MB720-9), CONDSTORE/QRESYNC
// delta tokens (MB720-8), $filter execution (MB720-6), SMTP submission, and
// connection pooling.
package imap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// Options configures the IMAP connection.
type Options struct {
	// TLS dials with implicit TLS (IMAPS). When false the connection is
	// plaintext — for local servers and tests only.
	TLS bool
}

// Client is an IMAP-backed mail.Backend over a single authenticated session.
type Client struct {
	c *imapclient.Client

	// mu guards onUnilateral, the watch callback the IDLE goroutine fires.
	// go-imap invokes the UnilateralDataHandler (installed at Dial time) from
	// an arbitrary goroutine, while Watch sets and clears onUnilateral; the
	// mutex makes that handoff race-free. A nil callback is a no-op, so the
	// handler is harmless outside a Watch.
	mu           sync.Mutex
	onUnilateral func()
	// vanished accumulates the UIDs the server reports expunged via QRESYNC
	// VANISHED responses (RFC 7162). Delta clears it before its FETCH and reads
	// it after to emit deletion tombstones. Guarded by mu, since the handler is
	// invoked from go-imap's read goroutine.
	vanished goimap.UIDSet
}

var _ mail.Backend = (*Client)(nil)

// Dial connects to addr (host:port), logs in, and returns a ready Client.
func Dial(addr, username, password string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{TLS: true}
	}
	cl := &Client{}
	// Install a unilateral-data handler at dial time (the only place go-imap
	// accepts one). It forwards EXISTS/EXPUNGE notifications to whatever callback
	// Watch has registered, guarded by cl.mu; with no Watch active the callback is
	// nil and the handler does nothing, leaving the read/write paths unchanged.
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			// Mailbox carries a NumMessages change on EXISTS (new mail).
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data != nil && data.NumMessages != nil {
					cl.fireUnilateral()
				}
			},
			// Expunge fires when a message is removed.
			Expunge: func(uint32) { cl.fireUnilateral() },
			// Vanished (QRESYNC) reports expunged UIDs: collect them for Delta to
			// turn into tombstones, and fire the watch trigger as Expunge would
			// (QRESYNC makes the server send VANISHED in place of EXPUNGE).
			Vanished: func(uids goimap.UIDSet, _ bool) {
				cl.mu.Lock()
				cl.vanished = append(cl.vanished, uids...)
				cl.mu.Unlock()
				cl.fireUnilateral()
			},
		},
	}
	var (
		c   *imapclient.Client
		err error
	)
	if o.TLS {
		c, err = imapclient.DialTLS(addr, opts)
	} else {
		c, err = imapclient.DialInsecure(addr, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", addr, err)
	}
	if err := c.Login(username, password).Wait(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	cl.c = c
	// Enable QRESYNC when the server offers it, so it reports expunges as
	// uid-based VANISHED responses that Delta turns into deletion tombstones.
	// Best-effort: a server without QRESYNC still serves every other path (only
	// Delta requires it, and says so).
	if c.Caps().Has(goimap.CapQResync) {
		if _, err := c.Enable(goimap.CapQResync).Wait(); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap: enable qresync: %w", err)
		}
	}
	return cl, nil
}

// fireUnilateral invokes the currently-registered watch callback, if any. It is
// called from go-imap's unilateral-data goroutine; the mutex serializes it with
// Watch installing and clearing the callback.
func (cl *Client) fireUnilateral() {
	cl.mu.Lock()
	cb := cl.onUnilateral
	cl.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// setUnilateral registers (or with nil clears) the watch callback.
func (cl *Client) setUnilateral(cb func()) {
	cl.mu.Lock()
	cl.onUnilateral = cb
	cl.mu.Unlock()
}

// Close logs out and closes the connection.
func (cl *Client) Close() error {
	if err := cl.c.Logout().Wait(); err != nil {
		_ = cl.c.Close()
		return err
	}
	return cl.c.Close()
}

// ListMailFolders enumerates the mailbox's folders with message counts.
func (cl *Client) ListMailFolders(_ context.Context) ([]mail.MailFolder, error) {
	items, err := cl.c.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: list: %w", err)
	}
	folders := make([]mail.MailFolder, 0, len(items))
	for _, it := range items {
		if it.Mailbox == "" || slices.Contains(it.Attrs, goimap.MailboxAttrNonExistent) {
			continue
		}
		f := mail.MailFolder{ID: folderID(it.Mailbox), DisplayName: displayName(it.Mailbox, it.Delim)}
		// A STATUS failure on one folder degrades to zero counts rather than
		// failing the whole listing.
		if st, err := cl.c.Status(it.Mailbox, &goimap.StatusOptions{NumMessages: true, NumUnseen: true}).Wait(); err == nil && st != nil {
			if st.NumMessages != nil {
				f.Total = int(*st.NumMessages)
			}
			if st.NumUnseen != nil {
				f.Unread = int(*st.NumUnseen)
			}
		}
		folders = append(folders, f)
	}
	return folders, nil
}

// ListMessages returns messages in a folder newest-first, bounded by page. Only
// envelope-level fields are populated (no body) — that is the cheap IMAP FETCH.
// An empty folderID selects the inbox, so Graph's /me/messages maps onto it.
//
// A non-nil filter restricts the result. The filter is run through the IMAP
// SEARCH command for any predicate that maps onto SEARCH criteria, then every
// candidate is re-checked client-side against the full filter AST so predicates
// IMAP cannot express still apply (see filter.go). Paging (Top/Skip, newest
// first) is applied after filtering.
func (cl *Client) ListMessages(_ context.Context, folderID string, page mail.Page, filter *odata.Filter) ([]mail.Message, error) {
	mailbox := "INBOX"
	if folderID != "" {
		var err error
		if mailbox, err = decodeFolderID(folderID); err != nil {
			return nil, err
		}
	}
	sel, err := cl.c.Select(mailbox, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: select %q: %w", mailbox, err)
	}
	if filter != nil && filter.Root != nil {
		return cl.listFiltered(mailbox, sel.UIDValidity, page, filter)
	}

	n := sel.NumMessages
	skip := uint32(max(page.Skip, 0))
	if n == 0 || skip >= n {
		return nil, nil
	}
	hi := n - skip
	lo := uint32(1)
	if page.Top > 0 && uint32(page.Top) < hi {
		lo = hi - uint32(page.Top) + 1
	}

	bufs, err := cl.c.Fetch(goimap.SeqSet{{Start: lo, Stop: hi}}, &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch: %w", err)
	}
	msgs := make([]mail.Message, 0, len(bufs))
	for _, b := range bufs {
		msgs = append(msgs, envelopeMessage(mailbox, sel.UIDValidity, b))
	}
	slices.Reverse(msgs) // FETCH yields ascending seq; Graph wants newest first
	return msgs, nil
}

// listFiltered lists messages matching filter. It narrows with an IMAP SEARCH
// built from the translatable part of the filter, then evaluates the full AST
// over each candidate's mapped mail.Message so non-translatable predicates still
// apply, and finally pages the survivors newest-first.
//
// The mailbox is assumed already selected by the caller.
func (cl *Client) listFiltered(mailbox string, uidValidity uint32, page mail.Page, filter *odata.Filter) ([]mail.Message, error) {
	criteria := translateFilter(filter.Root)

	data, err := cl.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imap: search: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return nil, nil
	}

	bufs, err := cl.c.Fetch(goimap.UIDSetNum(uids...), &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap: fetch: %w", err)
	}

	msgs := make([]mail.Message, 0, len(bufs))
	for _, b := range bufs {
		m := envelopeMessage(mailbox, uidValidity, b)
		// IMAP SEARCH is best-effort: re-check the full filter client-side so
		// predicates it could not express (or only approximated) are exact.
		if evalFilter(filter.Root, m) {
			msgs = append(msgs, m)
		}
	}
	// FETCH yields messages in ascending UID order; Graph wants newest first.
	slices.Reverse(msgs)
	return pageMessages(msgs, page), nil
}

// pageMessages applies Top/Skip to an already-ordered (newest first) slice.
func pageMessages(msgs []mail.Message, page mail.Page) []mail.Message {
	skip := max(page.Skip, 0)
	if skip >= len(msgs) {
		return nil
	}
	msgs = msgs[skip:]
	if page.Top > 0 && page.Top < len(msgs) {
		msgs = msgs[:page.Top]
	}
	return msgs
}

// GetMessage fetches a single message, including its parsed body, by opaque ID.
func (cl *Client) GetMessage(_ context.Context, id string) (mail.Message, error) {
	mailbox, uidValidity, uid, err := decodeMessageID(id)
	if err != nil {
		return mail.Message{}, err
	}
	sel, err := cl.c.Select(mailbox, nil).Wait()
	if err != nil {
		return mail.Message{}, fmt.Errorf("imap: select %q: %w", mailbox, err)
	}
	if sel.UIDValidity != uidValidity {
		return mail.Message{}, fmt.Errorf("imap: message id is stale: folder UIDVALIDITY changed")
	}

	bufs, err := cl.c.Fetch(goimap.UIDSetNum(goimap.UID(uid)), &goimap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
		// Peek so that fetching the body does not set \Seen — reading a message
		// must not mark it read.
		BodySection: []*goimap.FetchItemBodySection{{Peek: true}},
	}).Collect()
	if err != nil {
		return mail.Message{}, fmt.Errorf("imap: fetch: %w", err)
	}
	if len(bufs) == 0 {
		return mail.Message{}, fmt.Errorf("imap: message %s not found", id)
	}
	b := bufs[0]
	msg := envelopeMessage(mailbox, uidValidity, b)
	if len(b.BodySection) > 0 {
		msg.Body, msg.Preview, msg.HasAttachments = parseBody(b.BodySection[0].Bytes)
	}
	return msg, nil
}

// envelopeMessage maps a FETCH buffer's envelope-level data to a mail.Message.
func envelopeMessage(mailbox string, uidValidity uint32, b *imapclient.FetchMessageBuffer) mail.Message {
	m := mail.Message{
		ID:         messageID(mailbox, uidValidity, uint32(b.UID)),
		FolderID:   folderID(mailbox),
		ReceivedAt: b.InternalDate,
		IsRead:     slices.Contains(b.Flags, goimap.FlagSeen),
	}
	if env := b.Envelope; env != nil {
		m.Subject = env.Subject
		m.SentAt = env.Date
		m.From = firstAddress(env.From)
		m.To = addresses(env.To)
		m.Cc = addresses(env.Cc)
		m.Bcc = addresses(env.Bcc)
	}
	return m
}

// parseBody extracts a text/html body, a short preview, and attachment presence
// from a raw RFC 822 message. It is best-effort and never fails: a message that
// is not structured MIME falls back to its raw body, and parts in a charset
// go-message cannot decode are kept undecoded rather than dropped.
func parseBody(raw []byte) (mail.Body, string, bool) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		// Not structured MIME we can read: fall back to the bytes after the
		// header/body separator as plain text.
		text := rawBodyFallback(raw)
		return mail.Body{ContentType: "text", Content: text}, preview(text), false
	}
	defer func() { _ = mr.Close() }()

	var text, html string
	var hasAttach bool
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		// IsUnknownCharset is non-fatal — the part is still returned, just
		// undecoded. Any other error: stop and keep what we have so far.
		if err != nil && !gomessage.IsUnknownCharset(err) {
			break
		}
		switch h := p.Header.(type) {
		case *gomail.InlineHeader:
			data, readErr := io.ReadAll(p.Body)
			if readErr != nil {
				continue
			}
			switch ct, _, _ := h.ContentType(); ct {
			case "text/html":
				html = string(data)
			case "text/plain":
				text = string(data)
			}
		case *gomail.AttachmentHeader:
			hasAttach = true
		}
	}

	body := mail.Body{ContentType: "text", Content: text}
	if html != "" {
		body = mail.Body{ContentType: "html", Content: html}
	}
	p := text
	if p == "" {
		p = html
	}
	return body, preview(p), hasAttach
}

// rawBodyFallback returns the bytes after the first header/body separator.
func rawBodyFallback(raw []byte) string {
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if i := strings.Index(string(raw), sep); i >= 0 {
			return string(raw[i+len(sep):])
		}
	}
	return string(raw)
}

func preview(s string) string {
	return truncate(strings.TrimSpace(s), 255)
}

func address(a goimap.Address) mail.Address {
	email := a.Mailbox
	if a.Host != "" {
		email = a.Mailbox + "@" + a.Host
	}
	return mail.Address{Name: a.Name, Email: email}
}

func addresses(as []goimap.Address) []mail.Address {
	if len(as) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(as))
	for _, a := range as {
		out = append(out, address(a))
	}
	return out
}

func firstAddress(as []goimap.Address) mail.Address {
	if len(as) == 0 {
		return mail.Address{}
	}
	return address(as[0])
}

// displayName is the leaf segment of a hierarchical mailbox name.
func displayName(mailbox string, delim rune) string {
	if delim == 0 {
		return mailbox
	}
	if i := strings.LastIndexByte(mailbox, byte(delim)); i >= 0 {
		return mailbox[i+1:]
	}
	return mailbox
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

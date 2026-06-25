// Package jmap implements the mail.Backend port against a JMAP server (RFC 8620
// core + RFC 8621 mail) using git.sr.ht/~rockorager/go-jmap for the protocol.
//
// JMAP is the natural fit for the port: the port's neutral message/mailbox
// shapes are themselves Graph/JMAP-shaped, so this adapter is close to a
// pass-through. JMAP object IDs are already opaque, stable, server-assigned
// strings, so the port's opaque folder/message IDs carry them directly (base64
// wrapped only to keep them URL-safe and consistent with the IMAP adapter's
// posture). The delta sync-token wraps the JMAP Email state string.
//
// A Client is bound to one authenticated JMAP session for one mail account. The
// adapter covers folders, message listing/fetch, $filter translation to a
// native JMAP Email/query, read-state + delete writes, raw-message fetch, and
// Email/changes-backed delta. The JMAP push/EventSource watch (the IMAP IDLE
// analogue) is deferred — see the Watcher doc in internal/mail.
package jmap

import (
	"context"
	"fmt"
	"slices"
	"strings"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"git.sr.ht/~rockorager/go-jmap/mail"
	"git.sr.ht/~rockorager/go-jmap/mail/email"
	"git.sr.ht/~rockorager/go-jmap/mail/mailbox"

	port "github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// keywordSeen is the RFC 8621 keyword that marks a message read. Graph's isRead
// maps onto its presence in an Email's keywords map.
const keywordSeen = "$seen"

// emailGetProperties is the envelope-level Email property set fetched for
// listings and delta — everything mail.Message needs except the body, which
// GetMessage adds. Naming the properties keeps the FETCH cheap (the server need
// not compute body structure for a list).
var emailGetProperties = []string{
	"id", "mailboxIds", "subject", "from", "to", "cc", "bcc",
	"sentAt", "receivedAt", "preview", "keywords", "hasAttachment",
}

// Options configures the JMAP connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial
	// uses the base URL passed to Dial as the session endpoint.
	SessionEndpoint string
}

// Client is a JMAP-backed mail.Backend over one authenticated session and mail
// account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
}

var _ port.Backend = (*Client)(nil)

// Dial authenticates to the JMAP server at sessionURL with a bearer access
// token, fetches the Session, and resolves the primary mail account. The token
// is the operator's JMAP credential; the call site always sources it from an
// environment secret, never a flag.
func Dial(sessionURL, accessToken string, o *Options) (*Client, error) {
	if o == nil {
		o = &Options{}
	}
	endpoint := o.SessionEndpoint
	if endpoint == "" {
		endpoint = sessionURL
	}
	c := &gojmap.Client{SessionEndpoint: endpoint}
	c.WithAccessToken(accessToken)
	if err := c.Authenticate(); err != nil {
		return nil, fmt.Errorf("jmap: authenticate: %w", err)
	}
	accountID, ok := c.Session.PrimaryAccounts[mail.URI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("jmap: session advertises no primary mail account (%s)", mail.URI)
	}
	return &Client{c: c, accountID: accountID}, nil
}

// newClient wraps an already-configured go-jmap client and account id. It is the
// seam tests use to inject a client pointed at an httptest server.
func newClient(c *gojmap.Client, accountID gojmap.ID) *Client {
	return &Client{c: c, accountID: accountID}
}

// Close releases the session. JMAP is stateless HTTP with no logout, so there is
// nothing to tear down; the method exists to satisfy mail.Backend.
func (cl *Client) Close() error { return nil }

// do issues a one-call JMAP request and returns the single response invocation
// argument, surfacing a server MethodError as a Go error.
func (cl *Client) do(ctx context.Context, m gojmap.Method) (any, error) {
	req := &gojmap.Request{Context: ctx}
	req.Invoke(m)
	resp, err := cl.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jmap: request: %w", err)
	}
	if len(resp.Responses) == 0 {
		return nil, fmt.Errorf("jmap: empty response")
	}
	args := resp.Responses[0].Args
	if me, ok := args.(*gojmap.MethodError); ok {
		return nil, fmt.Errorf("jmap: method error: %w", me)
	}
	return args, nil
}

// ListMailFolders returns the account's mailboxes (JMAP folders/labels) with
// their message counts.
func (cl *Client) ListMailFolders(ctx context.Context) ([]port.MailFolder, error) {
	resp, err := cl.mailboxes(ctx)
	if err != nil {
		return nil, err
	}
	folders := make([]port.MailFolder, 0, len(resp.List))
	for _, mbox := range resp.List {
		folders = append(folders, port.MailFolder{
			ID:            folderID(mbox.ID),
			DisplayName:   mbox.Name,
			Total:         int(mbox.TotalEmails),
			Unread:        int(mbox.UnreadEmails),
			WellKnownName: wellKnownName(mbox.Role),
		})
	}
	return folders, nil
}

// mailboxes fetches every mailbox in the account (Mailbox/get with no ids).
func (cl *Client) mailboxes(ctx context.Context) (*mailbox.GetResponse, error) {
	args, err := cl.do(ctx, &mailbox.Get{Account: cl.accountID})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*mailbox.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response to Mailbox/get: %T", args)
	}
	return resp, nil
}

// ListMessages returns messages in a folder newest-first, bounded by page. Only
// envelope-level fields are populated (no body). An empty folderID selects the
// inbox (the mailbox with role "inbox"), so Graph's /me/messages maps onto it.
//
// A non-nil filter restricts the result. The translatable predicates are pushed
// into a native JMAP Email/query (which the server evaluates); paging is applied
// by the query's position/limit over a receivedAt-descending sort.
func (cl *Client) ListMessages(ctx context.Context, folderID string, page port.Page, filter *odata.Filter) ([]port.Message, error) {
	mailboxID, err := cl.resolveFolderID(ctx, folderID)
	if err != nil {
		return nil, err
	}

	cond := &email.FilterCondition{InMailbox: mailboxID}
	var f email.Filter = cond
	if filter != nil && filter.Root != nil {
		f = translateFilter(filter.Root, cond)
	}

	query := &email.Query{
		Account:  cl.accountID,
		Filter:   f,
		Sort:     []*email.SortComparator{{Property: "receivedAt", IsAscending: false}},
		Position: int64(max(page.Skip, 0)),
	}
	if page.Top > 0 {
		query.Limit = uint64(page.Top)
	}
	ids, err := cl.queryEmailIDs(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return cl.getEmails(ctx, ids, false)
}

// queryEmailIDs runs an Email/query and returns the matching email IDs in the
// query's sort order.
func (cl *Client) queryEmailIDs(ctx context.Context, query *email.Query) ([]gojmap.ID, error) {
	args, err := cl.do(ctx, query)
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*email.QueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response to Email/query: %T", args)
	}
	return resp.IDs, nil
}

// getEmails fetches the given email IDs and maps each to a port.Message,
// preserving the order of ids. When withBody is set the text/html body and a
// derived preview are populated (the GetMessage path); otherwise only
// envelope-level fields are.
func (cl *Client) getEmails(ctx context.Context, ids []gojmap.ID, withBody bool) ([]port.Message, error) {
	emails, err := cl.fetchEmails(ctx, ids, withBody)
	if err != nil {
		return nil, err
	}
	msgs := make([]port.Message, 0, len(emails))
	for _, e := range emails {
		msgs = append(msgs, mapEmail(e, withBody))
	}
	return msgs, nil
}

// fetchEmails runs an Email/get for ids and returns the raw Email objects in the
// order of ids (Email/get may return them in any order, so they are reindexed).
// withBody adds the body-value properties so a caller can read the message body.
func (cl *Client) fetchEmails(ctx context.Context, ids []gojmap.ID, withBody bool) ([]*email.Email, error) {
	get := &email.Get{
		Account:    cl.accountID,
		IDs:        ids,
		Properties: emailGetProperties,
	}
	if withBody {
		get.Properties = append(slices.Clone(emailGetProperties), "bodyValues", "textBody", "htmlBody")
		get.FetchTextBodyValues = true
		get.FetchHTMLBodyValues = true
	}
	args, err := cl.do(ctx, get)
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*email.GetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response to Email/get: %T", args)
	}
	byID := make(map[gojmap.ID]*email.Email, len(resp.List))
	for _, e := range resp.List {
		byID[e.ID] = e
	}
	out := make([]*email.Email, 0, len(ids))
	for _, id := range ids {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetMessage fetches a single message, including its parsed body, by opaque ID.
func (cl *Client) GetMessage(ctx context.Context, id string) (port.Message, error) {
	emailID, err := decodeMessageID(id)
	if err != nil {
		return port.Message{}, err
	}
	msgs, err := cl.getEmails(ctx, []gojmap.ID{emailID}, true)
	if err != nil {
		return port.Message{}, err
	}
	if len(msgs) == 0 {
		return port.Message{}, fmt.Errorf("jmap: message %s not found", id)
	}
	return msgs[0], nil
}

// resolveFolderID maps an opaque port folder id to a JMAP mailbox id. An empty
// id selects the inbox: the mailbox whose role is "inbox".
func (cl *Client) resolveFolderID(ctx context.Context, id string) (gojmap.ID, error) {
	if id != "" {
		return decodeFolderID(id)
	}
	return cl.inboxID(ctx)
}

// inboxID returns the JMAP id of the mailbox with role "inbox".
func (cl *Client) inboxID(ctx context.Context) (gojmap.ID, error) {
	resp, err := cl.mailboxes(ctx)
	if err != nil {
		return "", err
	}
	for _, mbox := range resp.List {
		if mbox.Role == mailbox.RoleInbox {
			return mbox.ID, nil
		}
	}
	return "", fmt.Errorf("jmap: no mailbox with role %q", mailbox.RoleInbox)
}

// mapEmail maps a JMAP Email to a port.Message. Envelope-level fields are always
// set; the body and a body-derived preview fallback are filled only when
// withBody is set (the GetMessage path). For listings the JMAP server's own
// preview is used.
func mapEmail(e *email.Email, withBody bool) port.Message {
	flagged, draft, categories := mapKeywords(e.Keywords)
	m := port.Message{
		ID:             messageID(e.ID),
		FolderID:       primaryFolderID(e.MailboxIDs),
		Subject:        e.Subject,
		From:           firstAddress(e.From),
		To:             addresses(e.To),
		Cc:             addresses(e.CC),
		Bcc:            addresses(e.BCC),
		Preview:        e.Preview,
		IsRead:         e.Keywords[keywordSeen],
		Flagged:        flagged,
		IsDraft:        draft,
		Categories:     categories,
		HasAttachments: e.HasAttachment,
	}
	if e.SentAt != nil {
		m.SentAt = *e.SentAt
	}
	if e.ReceivedAt != nil {
		m.ReceivedAt = *e.ReceivedAt
	}
	if withBody {
		m.Body = bodyOf(e)
		if m.Preview == "" {
			m.Preview = preview(m.Body.Content)
		}
	}
	return m
}

// bodyOf extracts a single-representation body from an Email, preferring HTML
// over plain text (matching the IMAP adapter), reading the part content out of
// the Email's bodyValues map keyed by partId.
func bodyOf(e *email.Email) port.Body {
	if html := bodyText(e, e.HTMLBody); html != "" {
		return port.Body{ContentType: "html", Content: html}
	}
	return port.Body{ContentType: "text", Content: bodyText(e, e.TextBody)}
}

// bodyText concatenates the bodyValues for the given body parts (almost always
// a single part), in order.
func bodyText(e *email.Email, parts []*email.BodyPart) string {
	var b strings.Builder
	for _, p := range parts {
		if v, ok := e.BodyValues[p.PartID]; ok {
			b.WriteString(v.Value)
		}
	}
	return b.String()
}

// primaryFolderID picks one mailbox id for a message that may belong to several
// (JMAP allows an email in multiple mailboxes; Graph's folderId is single-
// valued). The selection is deterministic — the lexicographically smallest id —
// so the same message always reports the same folder.
func primaryFolderID(ids map[gojmap.ID]bool) string {
	var best gojmap.ID
	for id, in := range ids {
		if in && (best == "" || id < best) {
			best = id
		}
	}
	if best == "" {
		return ""
	}
	return folderID(best)
}

func address(a *mail.Address) port.Address {
	if a == nil {
		return port.Address{}
	}
	return port.Address{Name: a.Name, Email: a.Email}
}

func addresses(as []*mail.Address) []port.Address {
	if len(as) == 0 {
		return nil
	}
	out := make([]port.Address, 0, len(as))
	for _, a := range as {
		out = append(out, address(a))
	}
	return out
}

func firstAddress(as []*mail.Address) port.Address {
	if len(as) == 0 {
		return port.Address{}
	}
	return address(as[0])
}

func preview(s string) string {
	return truncate(strings.TrimSpace(s), 255)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

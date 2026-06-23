package e2e

// imapfake_test.go is an in-process IMAP server (emersion/go-imap/v2 imapserver)
// that authenticates each connection via SASL OAUTHBEARER (RFC 7628). The
// OAUTHBEARER server mechanism (emersion/go-sasl) hands the fake the bearer token
// the mailboxd backend obtained by RFC 8693-exchanging the user token for the
// IMAP backend audience; the fake introspects it via a tokenValidator (RFC 7662),
// resolves the subject, and serves that subject's seeded INBOX from a userStore.
// This is the IMAP slice of the MB720-52 impersonation e2e: it proves the
// exchanged-token-to-mailbox binding over real IMAP wire commands without a real
// IMAP server.
//
// It is deliberately minimal: it implements only the commands the
// internal/mail/imap client issues to list /me/messages — AUTHENTICATE
// OAUTHBEARER, SELECT INBOX (and the STATUS the client may probe), and a
// sequence-number FETCH of ENVELOPE/FLAGS/INTERNALDATE/UID — and returns
// imapserver's "not implemented" error for everything else. Tokens are never
// logged.

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"
)

// startIMAPFake starts an in-process IMAP server on a free loopback port and
// returns its host:port. Each connection authenticates via SASL OAUTHBEARER,
// validating the bearer with v (and requiring the OAUTHBEARER authzid, when
// present, to equal the resulting subject) and then serving store.messages(sub).
// The server is stopped via t.Cleanup.
func startIMAPFake(t *testing.T, v *tokenValidator, store *userStore) (addr string) {
	t.Helper()
	addr = freeAddr(t)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &imapFakeSession{v: v, store: store}, nil, nil
		},
		// IMAP4rev2 keeps the server from emitting the obsolete RECENT/UNSEEN
		// responses and matches the modern go-imap client.
		Caps: imap.CapSet{imap.CapIMAP4rev2: {}},
		// The connection is plaintext loopback (mailboxd dials with
		// -mail-imap-tls=false), so SASL AUTHENTICATE must be allowed without TLS.
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("imap fake listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return addr
}

// imapFakeSession is one connection's session. It carries the validator and
// store, and once authenticated the resolved subject whose mailbox it serves.
type imapFakeSession struct {
	v     *tokenValidator
	store *userStore

	sub string // resolved subject after a successful OAUTHBEARER auth
}

var (
	_ imapserver.SessionSASL      = (*imapFakeSession)(nil)
	_ imapserver.SessionIMAP4rev2 = (*imapFakeSession)(nil)
)

// AuthenticateMechanisms advertises only OAUTHBEARER — the per-identity path the
// internal/mail/imap client uses when given a BearerToken.
func (s *imapFakeSession) AuthenticateMechanisms() []string {
	return []string{sasl.OAuthBearer}
}

// Authenticate returns the OAUTHBEARER server mechanism. Its callback introspects
// the bearer via the validator to resolve the subject, requiring the authzid (the
// OAUTHBEARER username, which the client sets to the subject) to match when
// present. On success the resolved subject is bound to the session.
func (s *imapFakeSession) Authenticate(mech string) (sasl.Server, error) {
	if mech != sasl.OAuthBearer {
		return nil, fmt.Errorf("imap fake: unsupported SASL mechanism %q", mech)
	}
	return sasl.NewOAuthBearerServer(func(opts sasl.OAuthBearerOptions) *sasl.OAuthBearerError {
		sub, err := s.v.validate(opts.Token)
		if err != nil {
			// Never log opts.Token.
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}
		if opts.Username != "" && opts.Username != sub {
			return &sasl.OAuthBearerError{Status: "invalid_token", Schemes: "bearer"}
		}
		s.sub = sub
		return nil
	}), nil
}

// Login rejects password auth: this fake is OAUTHBEARER-only.
func (s *imapFakeSession) Login(string, string) error {
	return imapserver.ErrAuthFailed
}

// Select serves the authenticated subject's INBOX. Any other mailbox is empty;
// the client only ever selects INBOX for /me/messages.
func (s *imapFakeSession) Select(mailbox string, _ *imap.SelectOptions) (*imap.SelectData, error) {
	n := s.numMessages(mailbox)
	return &imap.SelectData{
		NumMessages: n,
		UIDNext:     imap.UID(n + 1),
		UIDValidity: 1,
	}, nil
}

// Status reports the subject's INBOX counts. The client probes STATUS when
// enumerating folders; for /me/messages only NumMessages matters here.
func (s *imapFakeSession) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	n := s.numMessages(mailbox)
	data := &imap.StatusData{Mailbox: mailbox}
	if options.NumMessages {
		data.NumMessages = &n
	}
	if options.NumUnseen {
		data.NumUnseen = &n
	}
	if options.UIDNext {
		data.UIDNext = imap.UID(n + 1)
	}
	if options.UIDValidity {
		data.UIDValidity = 1
	}
	return data, nil
}

// Fetch writes one FETCH response per seeded message in the selected INBOX. The
// client lists /me/messages with a sequence-number FETCH of
// ENVELOPE/FLAGS/INTERNALDATE/UID; this populates exactly those. Sequence number
// i (1-based) maps to seeded message i-1 with UID i.
func (s *imapFakeSession) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	msgs := s.store.messages(s.sub)
	seqSet, ok := numSet.(imap.SeqSet)
	if !ok {
		// The client only uses a sequence-number FETCH for listing; anything else
		// is outside this fake's scope.
		return fmt.Errorf("imap fake: unsupported FETCH num set %T", numSet)
	}
	for i := range msgs {
		seqNum := uint32(i) + 1
		if !seqSet.Contains(seqNum) {
			continue
		}
		if err := s.writeMessage(w, seqNum, options, msgs[i]); err != nil {
			return err
		}
	}
	return nil
}

// writeMessage emits a single FETCH response for one seeded message.
func (s *imapFakeSession) writeMessage(w *imapserver.FetchWriter, seqNum uint32, options *imap.FetchOptions, m message) error {
	rw := w.CreateMessage(seqNum)
	if options.UID {
		rw.WriteUID(imap.UID(seqNum))
	}
	if options.Flags {
		rw.WriteFlags(nil)
	}
	if options.InternalDate {
		// A fixed, deterministic internal date; the assertion only checks subject.
		rw.WriteInternalDate(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	}
	if options.Envelope {
		rw.WriteEnvelope(&imap.Envelope{
			Subject: m.Subject,
			From:    []imap.Address{parseAddr(m.FromAddr)},
		})
	}
	return rw.Close()
}

// numMessages returns the count of seeded messages for the authenticated subject
// in mailbox; only INBOX (the client's listing target) is populated.
func (s *imapFakeSession) numMessages(mailbox string) uint32 {
	if !strings.EqualFold(mailbox, "INBOX") {
		return 0
	}
	return uint32(len(s.store.messages(s.sub)))
}

func (s *imapFakeSession) Unselect() error { return nil }
func (s *imapFakeSession) Close() error    { return nil }

// The remaining Session methods are unused by the /me/messages listing path; they
// return imapserver's "not implemented" error so any unexpected command fails
// loudly rather than silently misbehaving.
func (s *imapFakeSession) Create(string, *imap.CreateOptions) error { return errIMAPUnsupported }
func (s *imapFakeSession) Delete(string) error                      { return errIMAPUnsupported }
func (s *imapFakeSession) Rename(string, string, *imap.RenameOptions) error {
	return errIMAPUnsupported
}
func (s *imapFakeSession) Subscribe(string) error   { return errIMAPUnsupported }
func (s *imapFakeSession) Unsubscribe(string) error { return errIMAPUnsupported }
func (s *imapFakeSession) List(*imapserver.ListWriter, string, []string, *imap.ListOptions) error {
	return errIMAPUnsupported
}
func (s *imapFakeSession) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	return nil, errIMAPUnsupported
}
func (s *imapFakeSession) Poll(*imapserver.UpdateWriter, bool) error { return nil }
func (s *imapFakeSession) Idle(*imapserver.UpdateWriter, <-chan struct{}) error {
	return errIMAPUnsupported
}
func (s *imapFakeSession) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	return errIMAPUnsupported
}
func (s *imapFakeSession) Search(imapserver.NumKind, *imap.SearchCriteria, *imap.SearchOptions) (*imap.SearchData, error) {
	return nil, errIMAPUnsupported
}
func (s *imapFakeSession) Store(*imapserver.FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	return errIMAPUnsupported
}
func (s *imapFakeSession) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	return nil, errIMAPUnsupported
}

// Namespace and Move satisfy SessionIMAP4rev2 (required because the server
// advertises IMAP4rev2). The listing path never invokes them; Namespace reports
// the conventional single personal namespace, Move is unsupported.
func (s *imapFakeSession) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Delim: '/'}},
	}, nil
}

func (s *imapFakeSession) Move(*imapserver.MoveWriter, imap.NumSet, string) error {
	return errIMAPUnsupported
}

var errIMAPUnsupported = &imap.Error{
	Type: imap.StatusResponseTypeNo,
	Text: "command not supported by impersonation IMAP fake",
}

// parseAddr splits "local@host" into the IMAP envelope address parts. An address
// without "@" becomes the mailbox part with an empty host.
func parseAddr(addr string) imap.Address {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return imap.Address{Mailbox: addr[:i], Host: addr[i+1:]}
	}
	return imap.Address{Mailbox: addr}
}

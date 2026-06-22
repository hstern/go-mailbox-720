package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// Options configures the JMAP contacts connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial
	// uses the URL passed to Dial as the session endpoint.
	SessionEndpoint string
}

// Client is a JMAP-backed contacts.Backend over one authenticated session and
// contacts account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
}

var _ contacts.Backend = (*Client)(nil)

// Dial authenticates to the JMAP server at sessionURL with a bearer access token,
// fetches the Session, and resolves the primary contacts account. The token is the
// operator's JMAP credential; the call site always sources it from an environment
// secret, never a flag.
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
	accountID, ok := c.Session.PrimaryAccounts[contactsURI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("jmap: session advertises no primary contacts account (%s)", contactsURI)
	}
	return &Client{c: c, accountID: accountID}, nil
}

// newClient wraps an already-configured go-jmap client and account id — the seam
// tests use to inject a client pointed at an httptest server.
func newClient(c *gojmap.Client, accountID gojmap.ID) *Client {
	return &Client{c: c, accountID: accountID}
}

// Close releases the backend. The JMAP client is stateless over HTTP, so there is
// nothing to close.
func (cl *Client) Close() error { return nil }

// do issues a one-call JMAP request and returns the single response argument,
// surfacing a server MethodError as a Go error.
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

// ListAddressBooks returns the account's address books.
func (cl *Client) ListAddressBooks(ctx context.Context) ([]contacts.AddressBook, error) {
	args, err := cl.do(ctx, &addressBookGet{Account: cl.accountID})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*addressBookGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for AddressBook/get", args)
	}
	out := make([]contacts.AddressBook, 0, len(resp.List))
	for _, ab := range resp.List {
		out = append(out, contacts.AddressBook{ID: string(ab.ID), Name: ab.Name, Description: ab.Description})
	}
	return out, nil
}

// ListContacts returns the cards in an address book: ContactCard/query for the ids,
// then ContactCard/get to fetch them.
func (cl *Client) ListContacts(ctx context.Context, addressBookID string) ([]contacts.Contact, error) {
	qargs, err := cl.do(ctx, &cardQuery{Account: cl.accountID, Filter: &cardFilter{InAddressBook: gojmap.ID(addressBookID)}})
	if err != nil {
		return nil, err
	}
	qresp, ok := qargs.(*cardQueryResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for ContactCard/query", qargs)
	}
	if len(qresp.IDs) == 0 {
		return nil, nil
	}

	gargs, err := cl.do(ctx, &cardGet{Account: cl.accountID, IDs: qresp.IDs})
	if err != nil {
		return nil, err
	}
	gresp, ok := gargs.(*cardGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for ContactCard/get", gargs)
	}
	out := make([]contacts.Contact, 0, len(gresp.List))
	for _, cc := range gresp.List {
		out = append(out, contactFromCard(cc, addressBookID))
	}
	return out, nil
}

// GetContact fetches a single card by its JMAP id.
func (cl *Client) GetContact(ctx context.Context, id string) (contacts.Contact, error) {
	args, err := cl.do(ctx, &cardGet{Account: cl.accountID, IDs: []gojmap.ID{gojmap.ID(id)}})
	if err != nil {
		return contacts.Contact{}, err
	}
	resp, ok := args.(*cardGetResponse)
	if !ok {
		return contacts.Contact{}, fmt.Errorf("jmap: unexpected response %T for ContactCard/get", args)
	}
	if len(resp.List) == 0 {
		return contacts.Contact{}, fmt.Errorf("contact %q not found", id)
	}
	cc := resp.List[0]
	return contactFromCard(cc, firstAddressBookID(cc.AddressBookIDs)), nil
}

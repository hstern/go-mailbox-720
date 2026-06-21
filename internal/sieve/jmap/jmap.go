package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// ErrNoSieveAccount reports that the JMAP session does not advertise a primary
// account for Sieve scripts (urn:ietf:params:jmap:sieve) — the server does not
// support RFC 9661. A consumer can map it to "filters unsupported".
var ErrNoSieveAccount = errors.New("jmap: session advertises no primary sieve account")

// Options configures the JMAP Sieve connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial uses
	// the URL passed to Dial as the session endpoint.
	SessionEndpoint string
}

// Script is a SieveScript object's metadata; the content is fetched separately as a
// blob (see ScriptContent). It mirrors the RFC 9661 SieveScript: a server-assigned
// opaque ID, a per-account-unique Name, the BlobID of the script octets, and whether
// it is the account's single active script.
type Script struct {
	ID       string
	Name     string
	BlobID   string
	IsActive bool
}

// Client manages Sieve scripts over JMAP for one authenticated session and account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
}

// Dial authenticates to the JMAP server at sessionURL with a bearer access token,
// fetches the Session, and resolves the primary Sieve account (RFC 9661). The token
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
	return FromClient(c)
}

// FromClient wraps an already-authenticated go-jmap client as a Sieve transport,
// resolving the primary Sieve account from its session. It lets a consumer that
// already holds a JMAP session (e.g. the mail backend) manage Sieve scripts over the
// same connection rather than dialing a second one. It returns ErrNoSieveAccount
// when the session does not advertise Sieve support.
func FromClient(c *gojmap.Client) (*Client, error) {
	if c.Session == nil {
		return nil, fmt.Errorf("jmap: client is not authenticated")
	}
	accountID, ok := c.Session.PrimaryAccounts[sieveURI]
	if !ok || accountID == "" {
		return nil, ErrNoSieveAccount
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

func scriptFrom(s *sieveScript) Script {
	return Script{ID: string(s.ID), Name: s.Name, BlobID: string(s.BlobID), IsActive: s.IsActive}
}

// ListScripts returns all of the account's Sieve scripts (metadata only).
func (cl *Client) ListScripts(ctx context.Context) ([]Script, error) {
	args, err := cl.do(ctx, &sieveGet{Account: cl.accountID})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*sieveGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for SieveScript/get", args)
	}
	out := make([]Script, 0, len(resp.List))
	for _, s := range resp.List {
		out = append(out, scriptFrom(s))
	}
	return out, nil
}

// ScriptContent downloads the raw Sieve text of the blob.
func (cl *Client) ScriptContent(ctx context.Context, blobID string) (string, error) {
	rc, err := cl.c.DownloadWithContext(ctx, cl.accountID, gojmap.ID(blobID))
	if err != nil {
		return "", fmt.Errorf("jmap: download: %w", err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("jmap: read script: %w", err)
	}
	return string(b), nil
}

// ActiveScript returns the account's active script and its content. ok is false when
// no script is active.
func (cl *Client) ActiveScript(ctx context.Context) (script Script, content string, ok bool, err error) {
	scripts, err := cl.ListScripts(ctx)
	if err != nil {
		return Script{}, "", false, err
	}
	for _, s := range scripts {
		if s.IsActive {
			content, err := cl.ScriptContent(ctx, s.BlobID)
			if err != nil {
				return Script{}, "", false, err
			}
			return s, content, true, nil
		}
	}
	return Script{}, "", false, nil
}

// PutScript uploads content and creates a new (inactive) script named name. Activate
// it with Activate. A duplicate name is reported as an error.
func (cl *Client) PutScript(ctx context.Context, name, content string) (Script, error) {
	blobID, err := cl.uploadSieve(ctx, content)
	if err != nil {
		return Script{}, err
	}
	const cid = "s"
	args, err := cl.do(ctx, &sieveSet{
		Account: cl.accountID,
		Create:  map[string]*sieveScriptCreate{cid: {Name: name, BlobID: blobID}},
	})
	if err != nil {
		return Script{}, err
	}
	resp, ok := args.(*sieveSetResponse)
	if !ok {
		return Script{}, fmt.Errorf("jmap: unexpected response %T for SieveScript/set", args)
	}
	if se := resp.NotCreated[cid]; se != nil {
		return Script{}, fmt.Errorf("jmap: script not created: %s", setErrText(se))
	}
	created, ok := resp.Created[cid]
	if !ok {
		return Script{}, fmt.Errorf("jmap: SieveScript/set returned no created script")
	}
	// The server echoes only the members it assigns; fill the rest from the request.
	s := scriptFrom(created)
	if s.Name == "" {
		s.Name = name
	}
	if s.BlobID == "" {
		s.BlobID = string(blobID)
	}
	return s, nil
}

// UpdateScriptContent re-uploads content and points the existing script id at the
// new blob, leaving its name and active state unchanged. It returns only an error:
// a successful /set update reports no new server-set state worth surfacing (RFC 8620
// §5.3 lets the server return null for the updated entry), so callers that need the
// refreshed object re-read it via ListScripts.
func (cl *Client) UpdateScriptContent(ctx context.Context, id, content string) error {
	blobID, err := cl.uploadSieve(ctx, content)
	if err != nil {
		return err
	}
	jid := gojmap.ID(id)
	args, err := cl.do(ctx, &sieveSet{
		Account: cl.accountID,
		Update:  map[gojmap.ID]map[string]any{jid: {"blobId": blobID}},
	})
	if err != nil {
		return err
	}
	resp, ok := args.(*sieveSetResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response %T for SieveScript/set", args)
	}
	if se := resp.NotUpdated[jid]; se != nil {
		return fmt.Errorf("jmap: script not updated: %s", setErrText(se))
	}
	return nil
}

// SetActiveContent makes name the account's active script carrying content: it
// updates the script's content in place when a script of that name already exists
// (activating it if it was not active), and otherwise creates and activates it. It
// is the "publish this script as the mailbox's active filter" convenience a
// consumer drives on every rule change.
func (cl *Client) SetActiveContent(ctx context.Context, name, content string) error {
	scripts, err := cl.ListScripts(ctx)
	if err != nil {
		return err
	}
	for _, s := range scripts {
		if s.Name == name {
			if err := cl.UpdateScriptContent(ctx, s.ID, content); err != nil {
				return err
			}
			if s.IsActive {
				return nil
			}
			return cl.Activate(ctx, s.ID)
		}
	}
	created, err := cl.PutScript(ctx, name, content)
	if err != nil {
		return err
	}
	return cl.Activate(ctx, created.ID)
}

// Activate makes script id the account's single active script.
func (cl *Client) Activate(ctx context.Context, id string) error {
	jid := gojmap.ID(id)
	if err := cl.set(ctx, &sieveSet{Account: cl.accountID, ActivateScript: &jid}); err != nil {
		return err
	}
	return nil
}

// Deactivate clears the account's active script (no script runs on delivery).
func (cl *Client) Deactivate(ctx context.Context) error {
	return cl.set(ctx, &sieveSet{Account: cl.accountID, DeactivateScript: true})
}

// DeleteScript destroys script id, which must not be the active script.
func (cl *Client) DeleteScript(ctx context.Context, id string) error {
	jid := gojmap.ID(id)
	args, err := cl.do(ctx, &sieveSet{Account: cl.accountID, Destroy: []gojmap.ID{jid}})
	if err != nil {
		return err
	}
	resp, ok := args.(*sieveSetResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response %T for SieveScript/set", args)
	}
	if se := resp.NotDestroyed[jid]; se != nil {
		return fmt.Errorf("jmap: script not destroyed: %s", setErrText(se))
	}
	return nil
}

// Validate checks content for Sieve validity server-side (SieveScript/validate). It
// returns a non-nil error describing the problem when the server rejects the script.
func (cl *Client) Validate(ctx context.Context, content string) error {
	blobID, err := cl.uploadSieve(ctx, content)
	if err != nil {
		return err
	}
	args, err := cl.do(ctx, &sieveValidate{Account: cl.accountID, BlobID: blobID})
	if err != nil {
		return err
	}
	resp, ok := args.(*sieveValidateResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response %T for SieveScript/validate", args)
	}
	if resp.Error != nil {
		return fmt.Errorf("jmap: invalid sieve: %s", setErrText(resp.Error))
	}
	return nil
}

// set issues a SieveScript/set whose result the caller does not need to inspect
// beyond success (used by the activate/deactivate helpers).
func (cl *Client) set(ctx context.Context, m *sieveSet) error {
	args, err := cl.do(ctx, m)
	if err != nil {
		return err
	}
	if _, ok := args.(*sieveSetResponse); !ok {
		return fmt.Errorf("jmap: unexpected response %T for SieveScript/set", args)
	}
	return nil
}

// uploadSieve uploads content as an application/sieve blob (RFC 9661 §2.7) and
// returns its blob id. go-jmap's own Client.Upload hardcodes a Content-Type of
// application/json, so the upload is performed directly here against the session's
// upload URL with the correct media type.
func (cl *Client) uploadSieve(ctx context.Context, content string) (gojmap.ID, error) {
	// Resolve the upload URL under the client lock, mirroring go-jmap's own
	// UploadWithContext: Session can be (re)written by a concurrent Authenticate.
	cl.c.Lock()
	if cl.c.Session == nil {
		cl.c.Unlock()
		if err := cl.c.Authenticate(); err != nil {
			return "", fmt.Errorf("jmap: authenticate: %w", err)
		}
		cl.c.Lock()
	}
	url := strings.ReplaceAll(cl.c.Session.UploadURL, "{accountId}", string(cl.accountID))
	cl.c.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/sieve")
	resp, err := cl.c.HttpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jmap: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// JMAP Core (RFC 8620 §6.1) does not mandate a specific 2xx; some servers return
	// 201 Created. Accept any 2xx.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("jmap: upload: unexpected status %s", resp.Status)
	}
	var ur struct {
		BlobID gojmap.ID `json:"blobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		return "", fmt.Errorf("jmap: decode upload response: %w", err)
	}
	if ur.BlobID == "" {
		return "", fmt.Errorf("jmap: upload returned no blobId")
	}
	return ur.BlobID, nil
}

// setErrText renders a JMAP SetError as "type: description" (or just the type).
func setErrText(e *gojmap.SetError) string {
	if e == nil {
		return ""
	}
	if e.Description != nil {
		return fmt.Sprintf("%s: %s", e.Type, *e.Description)
	}
	return e.Type
}

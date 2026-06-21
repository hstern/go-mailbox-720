package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	managesieve "github.com/hstern/go-managesieve"

	port "github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mailfilter"
)

// This file implements the optional mail.FilterReader / mail.FilterWriter
// capabilities on the IMAP mail backend (MB720-19 chunk B). Inbox rules live in a
// Sieve script managed over ManageSieve (RFC 5804) — a separate connection from
// IMAP, typically on port 4190. The read-modify-write CRUD and the neutral-rule
// translation live in internal/mailfilter; this file dials/authenticates the
// ManageSieve connection and adapts it to mailfilter.ScriptStore. A fresh connection
// is opened per operation (dial → STARTTLS → SASL → work → logout); rule edits are
// infrequent, so the handshake cost is acceptable.

// rulesScriptName is the Sieve script this server owns and keeps active to hold the
// mailbox's inbox rules.
const rulesScriptName = "mailboxd"

var (
	_ port.FilterReader = (*Client)(nil)
	_ port.FilterWriter = (*Client)(nil)
)

// manageSieveConfig is the resolved ManageSieve connection configuration.
type manageSieveConfig struct {
	addr     string
	username string
	password string
	starttls bool
}

// resolveManageSieve turns the public options into the internal config, defaulting
// the SASL credentials to the IMAP ones. It returns nil (filters disabled) when no
// ManageSieve address is configured.
func resolveManageSieve(o *ManageSieveOptions, imapUser, imapPass string) *manageSieveConfig {
	if o == nil || o.Addr == "" {
		return nil
	}
	cfg := &manageSieveConfig{addr: o.Addr, username: o.Username, password: o.Password, starttls: o.STARTTLS}
	if cfg.username == "" {
		cfg.username = imapUser
	}
	if cfg.password == "" {
		cfg.password = imapPass
	}
	return cfg
}

// dialSieve opens and authenticates a ManageSieve connection, or reports
// mail.ErrFiltersUnsupported when none is configured. The caller logs out.
func (cl *Client) dialSieve(ctx context.Context) (*managesieve.Client, error) {
	if cl.sieve == nil {
		return nil, port.ErrFiltersUnsupported
	}
	ms, err := managesieve.Dial(ctx, cl.sieve.addr)
	if err != nil {
		return nil, fmt.Errorf("managesieve: dial %s: %w", cl.sieve.addr, err)
	}
	if cl.sieve.starttls {
		host, _, splitErr := net.SplitHostPort(cl.sieve.addr)
		if splitErr != nil {
			host = cl.sieve.addr
		}
		if err := ms.StartTLS(&tls.Config{ServerName: host}); err != nil {
			_ = ms.Close()
			return nil, fmt.Errorf("managesieve: starttls: %w", err)
		}
	}
	if err := ms.Authenticate(managesieve.PlainAuth("", cl.sieve.username, cl.sieve.password)); err != nil {
		_ = ms.Close()
		return nil, fmt.Errorf("managesieve: authenticate: %w", err)
	}
	return ms, nil
}

// sieveStore adapts a live ManageSieve connection to mailfilter.ScriptStore. It
// reads and writes the one script this server manages (rulesScriptName), so a
// pre-existing script the user keeps active elsewhere is never read or overwritten —
// only deactivated when our script is published.
type sieveStore struct{ ms *managesieve.Client }

// ActiveScript returns the content of this server's managed rules script, if it
// exists. It reads by name rather than "whatever is active", so an unrelated active
// script is left intact.
func (s sieveStore) ActiveScript(context.Context) (string, bool, error) {
	scripts, err := s.ms.ListScripts()
	if err != nil {
		return "", false, fmt.Errorf("managesieve: list scripts: %w", err)
	}
	for _, sc := range scripts {
		if sc.Name == rulesScriptName {
			body, err := s.ms.GetScript(sc.Name)
			if err != nil {
				return "", false, fmt.Errorf("managesieve: get script: %w", err)
			}
			return body, true, nil
		}
	}
	return "", false, nil
}

// SetActiveContent stores the content under the server's rules script and activates
// it.
func (s sieveStore) SetActiveContent(_ context.Context, content string) error {
	if _, err := s.ms.PutScript(rulesScriptName, content); err != nil {
		return fmt.Errorf("managesieve: put script: %w", err)
	}
	if err := s.ms.SetActive(rulesScriptName); err != nil {
		return fmt.Errorf("managesieve: set active: %w", err)
	}
	return nil
}

// withSieve opens a ManageSieve connection, runs fn against the script store over it,
// and logs out — so a whole read-modify-write happens on one connection.
func (cl *Client) withSieve(ctx context.Context, fn func(mailfilter.ScriptStore) error) error {
	ms, err := cl.dialSieve(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = ms.Logout() }()
	return fn(sieveStore{ms})
}

func (cl *Client) ListRules(ctx context.Context) ([]port.MessageRule, error) {
	var rules []port.MessageRule
	err := cl.withSieve(ctx, func(s mailfilter.ScriptStore) error {
		var e error
		rules, e = mailfilter.List(ctx, s)
		return e
	})
	return rules, err
}

func (cl *Client) GetRule(ctx context.Context, id string) (port.MessageRule, error) {
	var rule port.MessageRule
	err := cl.withSieve(ctx, func(s mailfilter.ScriptStore) error {
		var e error
		rule, e = mailfilter.Get(ctx, s, id)
		return e
	})
	return rule, err
}

func (cl *Client) CreateRule(ctx context.Context, rule port.MessageRule) (port.MessageRule, error) {
	var created port.MessageRule
	err := cl.withSieve(ctx, func(s mailfilter.ScriptStore) error {
		var e error
		created, e = mailfilter.Create(ctx, s, rule)
		return e
	})
	return created, err
}

func (cl *Client) UpdateRule(ctx context.Context, id string, rule port.MessageRule) (port.MessageRule, error) {
	var updated port.MessageRule
	err := cl.withSieve(ctx, func(s mailfilter.ScriptStore) error {
		var e error
		updated, e = mailfilter.Update(ctx, s, id, rule)
		return e
	})
	return updated, err
}

func (cl *Client) DeleteRule(ctx context.Context, id string) error {
	return cl.withSieve(ctx, func(s mailfilter.ScriptStore) error {
		return mailfilter.Delete(ctx, s, id)
	})
}

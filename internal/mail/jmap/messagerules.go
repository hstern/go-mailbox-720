package jmap

import (
	"context"
	"errors"
	"fmt"

	port "github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mailfilter"
	sievejmap "github.com/hstern/go-mailbox-720/internal/sieve/jmap"
)

// This file implements the optional mail.FilterReader / mail.FilterWriter
// capabilities on the JMAP mail backend (MB720-19 chunk C). Inbox rules live in the
// account's single active Sieve script, managed over JMAP for Sieve Scripts
// (RFC 9661) on the same session as the mail backend. The read-modify-write CRUD and
// the neutral-rule translation live in internal/mailfilter; this file only adapts the
// JMAP SieveScript transport to mailfilter.ScriptStore.

// rulesScriptName is the Sieve script this server owns and keeps active to hold the
// mailbox's inbox rules.
const rulesScriptName = "mailboxd"

var (
	_ port.FilterReader = (*Client)(nil)
	_ port.FilterWriter = (*Client)(nil)
)

// jmapStore adapts the JMAP SieveScript transport to mailfilter.ScriptStore. It
// reads and writes the one script this server manages (rulesScriptName), so a
// pre-existing script the user keeps active elsewhere is never read or overwritten —
// only deactivated when our script is published.
type jmapStore struct{ c *sievejmap.Client }

func (s jmapStore) ActiveScript(ctx context.Context) (string, bool, error) {
	scripts, err := s.c.ListScripts(ctx)
	if err != nil {
		return "", false, err
	}
	for _, sc := range scripts {
		if sc.Name == rulesScriptName {
			content, err := s.c.ScriptContent(ctx, sc.BlobID)
			if err != nil {
				return "", false, err
			}
			return content, true, nil
		}
	}
	return "", false, nil
}

func (s jmapStore) SetActiveContent(ctx context.Context, content string) error {
	return s.c.SetActiveContent(ctx, rulesScriptName, content)
}

// store wraps this backend's JMAP session as a script store, sharing the same
// connection and account session. It reports mail.ErrFiltersUnsupported when the
// server does not advertise the Sieve capability.
func (cl *Client) store() (mailfilter.ScriptStore, error) {
	sc, err := sievejmap.FromClient(cl.c)
	if err != nil {
		if errors.Is(err, sievejmap.ErrNoSieveAccount) {
			return nil, port.ErrFiltersUnsupported
		}
		return nil, fmt.Errorf("jmap: sieve: %w", err)
	}
	return jmapStore{sc}, nil
}

func (cl *Client) ListRules(ctx context.Context) ([]port.MessageRule, error) {
	s, err := cl.store()
	if err != nil {
		return nil, err
	}
	return mailfilter.List(ctx, s)
}

func (cl *Client) GetRule(ctx context.Context, id string) (port.MessageRule, error) {
	s, err := cl.store()
	if err != nil {
		return port.MessageRule{}, err
	}
	return mailfilter.Get(ctx, s, id)
}

func (cl *Client) CreateRule(ctx context.Context, rule port.MessageRule) (port.MessageRule, error) {
	s, err := cl.store()
	if err != nil {
		return port.MessageRule{}, err
	}
	return mailfilter.Create(ctx, s, rule)
}

func (cl *Client) UpdateRule(ctx context.Context, id string, rule port.MessageRule) (port.MessageRule, error) {
	s, err := cl.store()
	if err != nil {
		return port.MessageRule{}, err
	}
	return mailfilter.Update(ctx, s, id, rule)
}

func (cl *Client) DeleteRule(ctx context.Context, id string) error {
	s, err := cl.store()
	if err != nil {
		return err
	}
	return mailfilter.Delete(ctx, s, id)
}

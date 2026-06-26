package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	subjectid "github.com/hstern/go-subjectid"

	"github.com/hstern/go-mailbox-720/internal/auth"
	calendarjmap "github.com/hstern/go-mailbox-720/internal/calendar/jmap"
	jmapcontacts "github.com/hstern/go-mailbox-720/internal/contacts/jmap"
	mailjmap "github.com/hstern/go-mailbox-720/internal/mail/jmap"
	"github.com/hstern/go-mailbox-720/internal/notify"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

// watchReapInterval is how often the watch manager sweeps principals whose
// subscriptions have all expired and stops their watches.
const watchReapInterval = time.Minute

// watchManagerConfig carries what startWatchManager needs to build per-principal
// JMAP push watches in multi-tenant mode. A resource is watchable only when both
// its JMAP session URL and its RFC 8693 audience are configured (so each user's
// token can be exchanged for that backend).
type watchManagerConfig struct {
	exchanger tokenexchange.Exchanger

	mailSessionURL, mailAudience          string
	calSessionURL, calAudience, calAPIURL string
	contactsSessionURL, contactsAudience  string
}

// startWatchManager wires renewal-driven multi-tenant push: a per-principal watch
// manager re-armed by the bearer token each subscription create/renewal presents.
// It runs only when token exchange is configured (multi-tenant mode); in
// single-tenant mode the static notifiers handle delivery and this is a no-op.
//
// The handler stamps each subscription with the principal that created it and
// hands the manager that principal's token on every create/renewal, so the
// manager can (re)start a watch over the principal's own JMAP backend. The watch
// lives until the token expires; the next renewal re-arms it.
func startWatchManager(ctx context.Context, cfg watchManagerConfig, h *subscriptions.Handler, store subscriptions.Store) {
	if cfg.exchanger == nil {
		return
	}
	builders := cfg.builders()
	if len(builders) == 0 {
		log.Println("notifications(manager): disabled (no JMAP backend has both a session URL and an audience)")
		return
	}
	mgr := notify.NewManager(ctx, builders, store, subscriptions.GuardedClient(), time.Now, log.Printf)
	h.SetOwnerFunc(principalKeyOf)
	h.SetOnSubscribe(func(r *http.Request, owner string) {
		token, ok := auth.RawToken(r.Context())
		if !ok {
			return
		}
		mgr.OnSubscribe(owner, token)
	})
	startPoolReaper(ctx, watchReapInterval, mgr.Reap)
	log.Printf("notifications(manager): renewal-driven push enabled for %d resource(s)", len(builders))
}

// builders returns one notify.ResourceBuilder per JMAP resource that has both a
// session URL and an audience configured.
func (cfg watchManagerConfig) builders() []notify.ResourceBuilder {
	var out []notify.ResourceBuilder
	if cfg.mailSessionURL != "" && cfg.mailAudience != "" {
		out = append(out, mailWatchBuilder(cfg.exchanger, cfg.mailAudience, cfg.mailSessionURL))
	}
	if cfg.calSessionURL != "" && cfg.calAudience != "" {
		out = append(out, calendarWatchBuilder(cfg.exchanger, cfg.calAudience, cfg.calSessionURL, cfg.calAPIURL))
	}
	if cfg.contactsSessionURL != "" && cfg.contactsAudience != "" {
		out = append(out, contactsWatchBuilder(cfg.exchanger, cfg.contactsAudience, cfg.contactsSessionURL))
	}
	return out
}

// principalKeyOf returns a stable opaque key (iss + "|" + sub) for the request's
// authenticated principal, or "" when unauthenticated. It scopes both
// subscriptions (delivery) and watches to one principal.
func principalKeyOf(r *http.Request) string {
	id, ok := auth.Mailbox(r.Context())
	if !ok {
		return ""
	}
	switch v := id.(type) {
	case subjectid.IssSubID:
		return v.Iss + "|" + v.Sub
	case *subjectid.IssSubID:
		return v.Iss + "|" + v.Sub
	default:
		return ""
	}
}

// jmapWatchBuilder wraps a per-resource dial in the RFC 8693 token exchange that
// mints a backend token from the principal's bearer token. The returned builder
// reports the exchanged token's expiry so the manager bounds the watch by it.
func jmapWatchBuilder(resource string, ex tokenexchange.Exchanger, audience string,
	dial func(ctx context.Context, token string) (notify.WatchFunc, notify.SyncFunc, error)) notify.ResourceBuilder {
	return notify.ResourceBuilder{
		Resource: resource,
		Build: func(ctx context.Context, userToken string) (notify.WatchFunc, notify.SyncFunc, time.Time, bool, error) {
			tok, err := ex.Exchange(ctx, userToken, audience)
			if err != nil {
				return nil, nil, time.Time{}, false, fmt.Errorf("token exchange (%s): %w", audience, err)
			}
			watch, sync, err := dial(ctx, tok.AccessToken)
			if err != nil {
				return nil, nil, time.Time{}, false, err
			}
			return watch, sync, tok.ExpiresAt, true, nil
		},
	}
}

func mailWatchBuilder(ex tokenexchange.Exchanger, audience, sessionURL string) notify.ResourceBuilder {
	return jmapWatchBuilder(notify.MessagesResource, ex, audience, func(_ context.Context, token string) (notify.WatchFunc, notify.SyncFunc, error) {
		c, err := mailjmap.Dial(sessionURL, token, nil)
		if err != nil {
			return nil, nil, err
		}
		watch := func(ctx context.Context, onChange func()) error { return c.Watch(ctx, "", onChange) }
		sync := func(ctx context.Context, token string) ([]string, string, error) {
			msgs, _, next, err := c.Delta(ctx, "", token)
			if err != nil {
				return nil, "", err
			}
			ids := make([]string, len(msgs))
			for i, m := range msgs {
				ids[i] = m.ID
			}
			return ids, next, nil
		}
		return watch, sync, nil
	})
}

func calendarWatchBuilder(ex tokenexchange.Exchanger, audience, sessionURL, apiURL string) notify.ResourceBuilder {
	return jmapWatchBuilder(notify.EventsResource, ex, audience, func(ctx context.Context, token string) (notify.WatchFunc, notify.SyncFunc, error) {
		c, err := calendarjmap.Dial(sessionURL, token, &calendarjmap.Options{APIURLOverride: apiURL})
		if err != nil {
			return nil, nil, err
		}
		cals, err := c.ListCalendars(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(cals) == 0 {
			return nil, nil, fmt.Errorf("jmap: principal has no calendar to watch")
		}
		calID := cals[0].ID
		watch := func(ctx context.Context, onChange func()) error { return c.Watch(ctx, calID, onChange) }
		sync := func(ctx context.Context, token string) ([]string, string, error) {
			changed, _, next, err := c.Delta(ctx, calID, token)
			if err != nil {
				return nil, "", err
			}
			ids := make([]string, len(changed))
			for i, e := range changed {
				ids[i] = e.ID
			}
			return ids, next, nil
		}
		return watch, sync, nil
	})
}

func contactsWatchBuilder(ex tokenexchange.Exchanger, audience, sessionURL string) notify.ResourceBuilder {
	return jmapWatchBuilder(notify.ContactsResource, ex, audience, func(ctx context.Context, token string) (notify.WatchFunc, notify.SyncFunc, error) {
		c, err := jmapcontacts.Dial(sessionURL, token, nil)
		if err != nil {
			return nil, nil, err
		}
		books, err := c.ListAddressBooks(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(books) == 0 {
			return nil, nil, fmt.Errorf("jmap: principal has no address book to watch")
		}
		bookID := books[0].ID
		watch := func(ctx context.Context, onChange func()) error { return c.Watch(ctx, bookID, onChange) }
		sync := func(ctx context.Context, token string) ([]string, string, error) {
			changed, _, next, err := c.Delta(ctx, bookID, token)
			if err != nil {
				return nil, "", err
			}
			ids := make([]string, len(changed))
			for i, ct := range changed {
				ids[i] = ct.ID
			}
			return ids, next, nil
		}
		return watch, sync, nil
	})
}

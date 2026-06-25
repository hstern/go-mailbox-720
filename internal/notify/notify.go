// Package notify drives Microsoft Graph change-notification delivery from live
// backend changes. It watches a backend collection (a domain Watcher) and, on
// each change, computes the changed item ids (a domain DeltaReader) and POSTs
// notifications to the subscriptions watching that Graph resource
// (subscriptions.Notify).
//
// This is the delivery loop that ties the change-notification subscriptions
// (MB720-9) to a live backend. The mail entry point is Run (/me/messages); the
// resource-generic core RunResource serves any collection, so the calendar
// (/me/events) and contacts (/me/contacts) loops reuse it — over the JMAP
// WebSocket watch (RFC 8887, MB720-27) where the backend supports push. It is
// additive — it reports changed items (created), mirroring the DeltaReader's
// first-cut semantics; deletion/update notifications wait on a richer change
// model.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

// Graph resource collection paths the delivery loops notify for; subscriptions
// whose Resource matches one receive its notifications.
const (
	MessagesResource = "/me/messages"
	EventsResource   = "/me/events"
	ContactsResource = "/me/contacts"
)

// WatchFunc starts a change watch on a backend collection, invoking onChange
// (a coalesced signal) on each change until ctx is cancelled. It binds a domain
// Watcher to its collection id (inbox, primary calendar, default address book).
type WatchFunc func(ctx context.Context, onChange func()) error

// SyncFunc returns the ids changed since token together with the next token; an
// empty token is the initial baseline. It adapts a domain DeltaReader to the
// delivery loop, which needs only changed ids and a resource path, not the typed
// objects the DeltaReader returns.
type SyncFunc func(ctx context.Context, token string) (changedIDs []string, next string, err error)

// Run watches the inbox and delivers a "created" change notification to the
// matching subscriptions each time new mail arrives, until ctx is cancelled.
//
// watcher and syncer MUST be backed by SEPARATE connections: an IMAP IDLE watch
// monopolizes its connection, so the delta sync cannot run on the watched one.
// (A JMAP WebSocket watch uses its own socket and is exempt.) store and client
// are passed through to subscriptions.Notify (client should be the SSRF-guarded
// delivery client in production). now supplies the time used to skip expired
// subscriptions; report, if non-nil, is called with each delivery's Result so
// the caller can log outcomes. Run returns nil when ctx is cancelled, or a
// non-nil error if the initial baseline sync or the watch itself fails.
func Run(
	ctx context.Context,
	watcher mail.Watcher,
	syncer mail.DeltaReader,
	store subscriptions.Store,
	client *http.Client,
	now func() time.Time,
	report func(subscriptions.Result),
) error {
	watch := func(ctx context.Context, onChange func()) error {
		return watcher.Watch(ctx, "", onChange)
	}
	sync := func(ctx context.Context, token string) ([]string, string, error) {
		msgs, _, next, err := syncer.Delta(ctx, "", token)
		if err != nil {
			return nil, "", err
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		return ids, next, nil
	}
	return RunResource(ctx, "", MessagesResource, watch, sync, store, client, now, report)
}

// RunResource watches one Graph resource collection and delivers a "created"
// change notification to its matching subscriptions whenever the backing
// collection changes, until ctx is cancelled. resource is the Graph collection
// path (e.g. "/me/events"); watch and sync are the collection's adapted Watcher
// and DeltaReader. owner scopes delivery to one principal's subscriptions (empty
// in single-tenant mode); the per-principal watch manager passes the principal
// key here. The remaining parameters match Run. It returns nil on ctx
// cancellation, or an error if the baseline sync or the watch fails.
func RunResource(
	ctx context.Context,
	owner string,
	resource string,
	watch WatchFunc,
	sync SyncFunc,
	store subscriptions.Store,
	client *http.Client,
	now func() time.Time,
	report func(subscriptions.Result),
) error {
	if now == nil {
		now = time.Now
	}

	// Baseline: capture the current high-water mark without notifying for the
	// items already present when the loop starts. A change between this sync and
	// the watch starting is caught by the watch's first signal.
	_, token, err := sync(ctx, "")
	if err != nil {
		return fmt.Errorf("notify: baseline sync: %w", err)
	}

	// onChange runs on the watcher's single drain goroutine, so token is only
	// ever read/written from one goroutine — no synchronization needed.
	onChange := func() {
		ids, next, err := sync(ctx, token)
		if err != nil {
			// A transient sync error: keep the old token and retry on the next
			// signal rather than advancing past unseen items.
			if report != nil {
				report(subscriptions.Result{Errors: map[string]error{"": fmt.Errorf("notify: sync: %w", err)}})
			}
			return
		}
		token = next
		if len(ids) == 0 {
			return
		}
		res := subscriptions.Notify(ctx, client, store, subscriptions.Change{
			Resource:    resource,
			ChangeType:  subscriptions.ChangeCreated,
			ResourceIDs: ids,
			Owner:       owner,
		}, now())
		if report != nil {
			report(res)
		}
	}

	if err := watch(ctx, onChange); err != nil {
		return fmt.Errorf("notify: watch: %w", err)
	}
	return nil
}

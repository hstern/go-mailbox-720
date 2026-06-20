// Package notify drives Microsoft Graph change-notification delivery from IMAP
// inbox changes. It watches the inbox (mail.Watcher) and, on each change,
// computes the newly-arrived messages (mail.DeltaReader) and POSTs notifications
// to the subscriptions watching /me/messages (subscriptions.Notify).
//
// This is the delivery loop that ties the change-notification subscriptions
// (MB720-9) to a live mailbox. It is additive — it reports newly-arrived messages
// (created), mirroring the DeltaReader's first-cut semantics; deletion/update
// notifications wait on CONDSTORE delta (MB720-8) and a richer change model.
package notify

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
)

// MessagesResource is the Graph resource path the inbox delivers notifications
// for; subscriptions whose Resource matches receive them.
const MessagesResource = "/me/messages"

// Run watches the inbox and delivers a "created" change notification to the
// matching subscriptions each time new mail arrives, until ctx is cancelled.
//
// watcher and syncer MUST be backed by SEPARATE connections: an IMAP IDLE watch
// monopolizes its connection, so the delta sync cannot run on the watched one.
// store and client are passed through to subscriptions.Notify (client should be
// the SSRF-guarded delivery client in production). now supplies the time used to
// skip expired subscriptions; report, if non-nil, is called with each delivery's
// Result so the caller can log outcomes. Run returns nil when ctx is cancelled,
// or a non-nil error if the initial baseline sync or the watch itself fails.
func Run(
	ctx context.Context,
	watcher mail.Watcher,
	syncer mail.DeltaReader,
	store subscriptions.Store,
	client *http.Client,
	now func() time.Time,
	report func(subscriptions.Result),
) error {
	if now == nil {
		now = time.Now
	}

	// Baseline: capture the current high-water mark without notifying for the
	// messages already present when the loop starts. A change between this sync
	// and the watch starting is caught by the watch's first signal.
	_, token, err := syncer.Delta(ctx, "", "")
	if err != nil {
		return fmt.Errorf("notify: baseline sync: %w", err)
	}

	// onChange runs on the watcher's single drain goroutine, so the token is only
	// ever read/written from one goroutine — no synchronization needed.
	onChange := func() {
		msgs, next, err := syncer.Delta(ctx, "", token)
		if err != nil {
			// A transient sync error: keep the old token and retry on the next
			// signal rather than advancing past unseen messages.
			if report != nil {
				report(subscriptions.Result{Errors: map[string]error{"": fmt.Errorf("notify: sync: %w", err)}})
			}
			return
		}
		token = next
		if len(msgs) == 0 {
			return
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		res := subscriptions.Notify(ctx, client, store, subscriptions.Change{
			Resource:    MessagesResource,
			ChangeType:  subscriptions.ChangeCreated,
			ResourceIDs: ids,
		}, now())
		if report != nil {
			report(res)
		}
	}

	if err := watcher.Watch(ctx, "", onChange); err != nil {
		return fmt.Errorf("notify: watch: %w", err)
	}
	return nil
}

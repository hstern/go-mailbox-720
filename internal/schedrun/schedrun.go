// Package schedrun runs the inbound iTIP/iMIP scheduling trigger off IMAP inbox
// changes. It watches the inbox (mail.Watcher) and, on each change, pulls the new
// messages (mail.DeltaReader), fetches each in raw form (mail.RawReader), and
// turns any that are iMIP METHOD:REQUEST invitations into tentative calendar
// events (itip.ProcessRequest). Ordinary mail and non-REQUEST scheduling messages
// are skipped.
//
// This is the trigger loop for the dumb-backend scheduling case (MB720-10),
// mirroring internal/notify's subscription delivery loop. First cut and its
// deliberate limits:
//
//   - It surfaces invitations as tentative events only; it does NOT auto-reply.
//     An inbound REQUEST is attacker-influenceable mail, so turning it into an
//     outbound SMTP send must stay a user action (itip.Respond), never automatic.
//   - It always runs when a calendar backend is present — the RFC 6638 capability
//     switch (delegate to a scheduling-aware CalDAV server) is future work.
//   - It always creates; UID-correlated updates/cancellations of an existing
//     tentative event (a re-sent REQUEST, a CANCEL) are future work.
//   - Idempotency comes from the delta baseline: only messages that arrive after
//     the loop starts are processed, each once; a restart re-baselines without
//     reprocessing existing mail (an IMAP-keyword marker is the durable fix).
package schedrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/itip"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/scheduling"
)

// Run watches the inbox and creates a tentative event for each inbound iMIP
// REQUEST until ctx is cancelled.
//
// watcher, syncer, and raw MAY share one connection only if it is not the watched
// one: an IMAP IDLE watch monopolizes its connection, so watcher must be a
// dedicated connection while syncer/raw run on another. w is the calendar the
// tentative events are created in (calendarID names the collection). report, if
// non-nil, is called for each processed message: with the created event and a nil
// error on success, or a zero event and a non-nil error on a real failure
// (a malformed invite, a backend error). Ordinary mail and non-REQUEST scheduling
// messages are skipped silently and never reported. Run returns nil when ctx is
// cancelled, or an error if the baseline sync or the watch itself fails.
func Run(
	ctx context.Context,
	watcher mail.Watcher,
	syncer mail.DeltaReader,
	raw mail.RawReader,
	w calendar.Writer,
	calendarID string,
	report func(calendar.Event, error),
) error {
	// Baseline: capture the current high-water mark so pre-existing mail isn't
	// (re)processed; only messages arriving after this are turned into events.
	_, token, err := syncer.Delta(ctx, "", "")
	if err != nil {
		return fmt.Errorf("schedrun: baseline sync: %w", err)
	}

	// onChange runs on the watcher's single drain goroutine, so token is only ever
	// touched from one goroutine — no synchronization needed.
	onChange := func() {
		msgs, next, err := syncer.Delta(ctx, "", token)
		if err != nil {
			// Keep the old token and retry on the next signal rather than skipping
			// unseen messages.
			if report != nil {
				report(calendar.Event{}, fmt.Errorf("schedrun: sync: %w", err))
			}
			return
		}
		token = next
		for _, m := range msgs {
			rawBytes, err := raw.RawMessage(ctx, m.ID)
			if err != nil {
				if report != nil {
					report(calendar.Event{}, fmt.Errorf("schedrun: fetch %s: %w", m.ID, err))
				}
				continue
			}
			event, err := itip.ProcessRequest(ctx, w, calendarID, rawBytes)
			if err != nil {
				// Ordinary mail (no calendar part) or a non-REQUEST scheduling
				// message: not for us, skip silently.
				if errors.Is(err, scheduling.ErrNoCalendar) || errors.Is(err, itip.ErrNotRequest) {
					continue
				}
				// A genuine failure: a malformed invite or a backend write error.
				if report != nil {
					report(calendar.Event{}, err)
				}
				continue
			}
			if report != nil {
				report(event, nil)
			}
		}
	}

	if err := watcher.Watch(ctx, "", onChange); err != nil {
		return fmt.Errorf("schedrun: watch: %w", err)
	}
	return nil
}

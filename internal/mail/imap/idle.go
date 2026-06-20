package imap

import (
	"context"
	"errors"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/mail"
)

var _ mail.Watcher = (*Client)(nil)

// Watch blocks until ctx is cancelled (or an error occurs), invoking onChange
// each time folderID changes (a message arrives or is removed). An empty
// folderID watches the inbox. It is the backing for the change-notification
// delivery loop (MB720-9) and the scheduling trigger (MB720-10).
//
// onChange is a coalesced signal, not a description of what changed: the caller
// re-syncs (e.g. via Delta) to discover specifics. Rapid bursts of server
// notifications collapse into at most one pending onChange, so a flood of
// arrivals does not spin a tight loop.
//
// IDLE monopolizes the connection — while Watch runs no other command can be
// issued on this Client's session. A Watcher should therefore own a dedicated
// Client (see the mail.Watcher doc).
//
// Mechanics: the folder is SELECTed, then go-imap's IDLE is started. The
// unilateral-data handler installed at Dial time (forwarding EXISTS/EXPUNGE)
// is pointed, for the duration of the watch, at an internal signal channel; a
// drain goroutine turns those signals into coalesced onChange calls. IDLE
// blocks until ctx is done, at which point the IdleCommand is Closed so Wait
// returns. go-imap auto-restarts IDLE under the hood to dodge server inactivity
// timeouts, so a single Idle()/Wait() spans the whole watch.
func (cl *Client) Watch(ctx context.Context, folderID string, onChange func()) error {
	mailbox := "INBOX"
	if folderID != "" {
		var err error
		if mailbox, err = decodeFolderID(folderID); err != nil {
			return err
		}
	}
	if _, err := cl.c.Select(mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("imap: watch: select %q: %w", mailbox, err)
	}

	// signal is buffered (capacity 1) so the unilateral-data handler — which
	// runs on go-imap's connection goroutine and blocks the client while it
	// runs — never blocks: a notification that arrives while one is already
	// pending is simply coalesced. The drain goroutine converts signals into
	// onChange calls, keeping any slow onChange off the IMAP read path.
	signal := make(chan struct{}, 1)
	cl.setUnilateral(func() {
		select {
		case signal <- struct{}{}:
		default: // a change is already pending; coalesce
		}
	})
	defer cl.setUnilateral(nil)

	// stop is closed when Watch is unwinding (cancellation or IDLE error); both
	// helper goroutines watch it so neither leaks past Watch's return.
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			select {
			case <-stop:
				return
			case <-signal:
				onChange()
			}
		}
	}()

	idle, err := cl.c.Idle()
	if err != nil {
		return fmt.Errorf("imap: watch: idle: %w", err)
	}

	// Closer: Idle.Wait blocks until the command ends, so a second goroutine
	// closes the command when ctx is cancelled (the normal stop) or when Wait
	// has already returned on its own (a real error, signalled via stop).
	go func() {
		select {
		case <-ctx.Done():
			_ = idle.Close()
		case <-stop:
		}
	}()

	waitErr := idle.Wait()

	if ctx.Err() != nil {
		// Cancellation is the normal stop signal, not an error.
		return nil
	}
	if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		return fmt.Errorf("imap: watch: %w", waitErr)
	}
	return nil
}

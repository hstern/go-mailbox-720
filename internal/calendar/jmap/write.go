package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
	"github.com/hstern/go-jscalendar"
	jscal "github.com/hstern/go-jscalendar/jmap"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

// Client implements calendar.Writer in addition to calendar.Backend, so a
// consumer can type-assert the backend to calendar.Writer to reach the event
// write paths.
var _ calendar.Writer = (*Client)(nil)

// Client also implements calendar.InstanceWriter so that a consumer can
// type-assert for per-occurrence override writes without these bleeding into the
// core Writer interface.
var _ calendar.InstanceWriter = (*Client)(nil)

// CreateEvent creates a new event in the named calendar and returns it as
// stored. It calls fromCalendarEvent to map the neutral calendar.Event to a
// CalendarEvent, sets CalendarIDs from calendarID, then sends a
// CalendarEvent/set with Create and SendSchedulingMessages=true so that the
// server dispatches iTIP invitations. If the server rejects the create via
// notCreated the error is surfaced; if the server returns a null created
// object (RFC 8620 §5.3 allows this when there are no extra fields to echo),
// the server-assigned id is not recoverable and we return the input event with
// an empty ID — callers needing the id must re-fetch.
func (cl *Client) CreateEvent(ctx context.Context, calendarID string, e calendar.Event) (calendar.Event, error) {
	ce, err := fromCalendarEvent(e)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("jmap: CreateEvent: %w", err)
	}
	// Override CalendarIDs from the explicit calendarID argument.
	ce.CalendarIDs = map[jscalendar.Id]bool{jscalendar.Id(calendarID): true}

	args, err := cl.do(ctx, &eventSet{
		Account:                cl.accountID,
		Create:                 map[gojmap.ID]*jscal.CalendarEvent{"new": ce},
		SendSchedulingMessages: true,
	})
	if err != nil {
		return calendar.Event{}, err
	}
	resp, ok := args.(*eventSetResponse)
	if !ok {
		return calendar.Event{}, fmt.Errorf("jmap: unexpected response to CalendarEvent/set: %T", args)
	}
	if se, ok := resp.NotCreated["new"]; ok {
		return calendar.Event{}, fmt.Errorf("jmap: set: %s", setErrorString(se))
	}

	// RFC 8620 §5.3: the created map MAY contain a null value for "new" when
	// the server does not echo back any fields. The server-assigned id is not
	// recoverable from a null created object; return the input event with an
	// empty ID. Callers needing the id must re-fetch.
	created := resp.Created["new"]
	if created == nil {
		// No echo — return the input event as-is (ID will be empty).
		return e, nil
	}
	return toCalendarEvent(created)
}

// UpdateEvent replaces the event identified by e.ID with e and returns the
// updated event. It sends a CalendarEvent/set update patch built from the full
// CalendarEvent representation, mirroring how mail/jmap/write.go builds an
// Email/set update patch (a gojmap.Patch = map[string]interface{} with one
// entry per top-level property). If the server rejects the update via
// notUpdated the error is surfaced; a null updated entry falls back to the
// input event.
func (cl *Client) UpdateEvent(ctx context.Context, e calendar.Event) (calendar.Event, error) {
	ce, err := fromCalendarEvent(e)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("jmap: UpdateEvent: %w", err)
	}

	// Build a per-property patch from the CalendarEvent. This mirrors the
	// mail/jmap/write.go approach for Email/set update: each top-level property
	// is expressed as a separate JSON-pointer key, which allows the server to
	// apply a partial update rather than a blind replace. For a CalendarEvent
	// the top-level properties that fromCalendarEvent populates are serialised
	// into the patch map via patchFromCalendarEvent.
	patch := patchFromCalendarEvent(ce)

	id := gojmap.ID(e.ID)
	args, err := cl.do(ctx, &eventSet{
		Account:                cl.accountID,
		Update:                 map[gojmap.ID]gojmap.Patch{id: patch},
		SendSchedulingMessages: true,
	})
	if err != nil {
		return calendar.Event{}, err
	}
	resp, ok := args.(*eventSetResponse)
	if !ok {
		return calendar.Event{}, fmt.Errorf("jmap: unexpected response to CalendarEvent/set: %T", args)
	}
	if se, ok := resp.NotUpdated[id]; ok {
		return calendar.Event{}, fmt.Errorf("jmap: set: %s", setErrorString(se))
	}

	// RFC 8620 §5.3: the updated map entry may be null when the server does not
	// echo back changed fields. Fall back to the input event in that case.
	updated := resp.Updated[id]
	if updated == nil {
		return e, nil
	}
	return toCalendarEvent(updated)
}

// DeleteEvent removes the event identified by id, the backing for Graph's
// DELETE semantics. It sends a CalendarEvent/set destroy and surfaces any
// server-side rejection via notDestroyed.
func (cl *Client) DeleteEvent(ctx context.Context, id string) error {
	jmapID := gojmap.ID(id)
	args, err := cl.do(ctx, &eventSet{
		Account:                cl.accountID,
		Destroy:                []gojmap.ID{jmapID},
		SendSchedulingMessages: true,
	})
	if err != nil {
		return err
	}
	resp, ok := args.(*eventSetResponse)
	if !ok {
		return fmt.Errorf("jmap: unexpected response to CalendarEvent/set: %T", args)
	}
	if se, ok := resp.NotDestroyed[jmapID]; ok {
		return fmt.Errorf("jmap: set: %s", setErrorString(se))
	}
	return nil
}

// setErrorString formats a *gojmap.SetError into a human-readable string,
// mirroring the identical helper in mail/jmap/write.go verbatim.
func setErrorString(se *gojmap.SetError) string {
	if se == nil {
		return "unknown set error"
	}
	if se.Description != nil {
		return se.Type + ": " + *se.Description
	}
	return se.Type
}

// WriteInstanceOverride adds or replaces a per-occurrence override for the
// recurring series identified by masterID. override must carry a non-zero
// RecurrenceID (which occurrence to override) and a non-empty ID.
//
// # Synthetic instance id design
//
// JMAP calendars (RFC 8984 / JMAP Calendars draft) does not expose per-occurrence
// ids natively in the same way CalDAV does: a JMAP server stores recurrence
// overrides inside the master CalendarEvent's recurrenceOverrides map, keyed by
// the recurrence date-time, and may mint a synthetic per-instance id for
// ListInstances responses. This implementation mirrors how GetEvent and
// ListInstances mint synthetic ids (the format is an implementation detail of the
// JMAP server): when a caller has obtained an instance from those paths it
// already holds the server's synthetic id in Event.ID. WriteInstanceOverride
// requires the caller to pass that id in override.ID — it is used directly as the
// JMAP CalendarEvent id in the Update map. The server interprets an Update on a
// synthetic instance id as a patch to the corresponding recurrenceOverrides entry
// of the master event, leaving all other occurrences and the master rule intact.
// Callers that do not yet hold the synthetic id must call GetEvent or
// ListInstances first to obtain it.
//
// On success the returned event is stamped with IsOverride=true. If the server
// echoes a nil updated entry (RFC 8620 §5.3 permits this) the input override is
// returned with IsOverride set.
func (cl *Client) WriteInstanceOverride(ctx context.Context, masterID string, override calendar.Event) (calendar.Event, error) {
	// masterID is accepted for interface symmetry; the update is keyed by the synthetic instance id (override.ID), from which the server resolves the series master.
	if override.RecurrenceID == nil {
		return calendar.Event{}, fmt.Errorf("jmap: WriteInstanceOverride requires a non-zero RecurrenceID")
	}
	if override.ID == "" {
		return calendar.Event{}, fmt.Errorf("jmap: WriteInstanceOverride requires the synthetic instance id in override.ID; call GetEvent or ListInstances first to obtain it")
	}

	ce, err := fromCalendarEvent(override)
	if err != nil {
		return calendar.Event{}, fmt.Errorf("jmap: WriteInstanceOverride: %w", err)
	}
	patch := patchFromCalendarEvent(ce)

	id := gojmap.ID(override.ID)
	args, err := cl.do(ctx, &eventSet{
		Account:                cl.accountID,
		Update:                 map[gojmap.ID]gojmap.Patch{id: patch},
		SendSchedulingMessages: true,
	})
	if err != nil {
		return calendar.Event{}, err
	}
	resp, ok := args.(*eventSetResponse)
	if !ok {
		return calendar.Event{}, fmt.Errorf("jmap: unexpected response to CalendarEvent/set: %T", args)
	}
	if se, ok := resp.NotUpdated[id]; ok {
		return calendar.Event{}, fmt.Errorf("jmap: set: %s", setErrorString(se))
	}

	// RFC 8620 §5.3: the updated map entry may be null when the server does not
	// echo back changed fields. Fall back to the input override in that case.
	updated := resp.Updated[id]
	if updated == nil {
		override.IsOverride = true
		return override, nil
	}
	result, err := toCalendarEvent(updated)
	if err != nil {
		return calendar.Event{}, err
	}
	result.IsOverride = true
	return result, nil
}

// patchFromCalendarEvent converts a *jscal.CalendarEvent into a gojmap.Patch
// (map[string]interface{}) with one entry per top-level property the neutral
// model carries on its embedded JSCalendar Event. This mirrors how Email/set
// updates in mail/jmap/write.go express per-property JSON-pointer patches so
// that the server performs a partial update rather than a blind replace.
//
// The property set targets the embedded jscalendar.Event fields directly
// (title/start/status/…), preserving the same patch paths the JMAP server
// expects. uid is immutable per RFC 8984 §4.1.1 and sequence is server-managed
// under delegated scheduling, so both are intentionally omitted from patches.
func patchFromCalendarEvent(ce *jscal.CalendarEvent) gojmap.Patch {
	p := make(gojmap.Patch)
	if ce == nil {
		return p
	}

	// CalendarEvent-level (JMAP) properties.
	p["calendarIds"] = ce.CalendarIDs
	if ce.BaseEventID != nil {
		p["baseEventId"] = ce.BaseEventID
	}

	if ce.Event == nil {
		return p
	}

	// JSCalendar Event content properties.
	p["title"] = ce.Title
	p["status"] = ce.Status
	p["showWithoutTime"] = ce.ShowWithoutTime
	p["description"] = ce.Description
	p["descriptionContentType"] = ce.DescriptionContentType

	if ce.Created != nil {
		p["created"] = ce.Created
	}
	if ce.Start != nil {
		p["start"] = ce.Start
		p["timeZone"] = ce.TimeZone
	}
	if ce.Duration != nil {
		p["duration"] = ce.Duration
	}

	p["locations"] = ce.Locations
	p["participants"] = ce.Participants

	if ce.RecurrenceRules != nil {
		p["recurrenceRules"] = ce.RecurrenceRules
	}
	if ce.ExcludedRecurrenceRules != nil {
		p["excludedRecurrenceRules"] = ce.ExcludedRecurrenceRules
	}
	if ce.RecurrenceOverrides != nil {
		p["recurrenceOverrides"] = ce.RecurrenceOverrides
	}

	// Override-instance linkage: present only on exception events.
	if ce.RecurrenceID != nil {
		p["recurrenceId"] = ce.RecurrenceID
		p["recurrenceIdTimeZone"] = ce.RecurrenceIDTimeZone
	}

	return p
}

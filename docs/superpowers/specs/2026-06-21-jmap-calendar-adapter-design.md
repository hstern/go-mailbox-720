# JMAP Calendar adapter — design (MB720-20)

**Status:** approved design, pre-implementation
**Issue:** MB720-20 "JMAP Calendar adapter (draft-ietf-jmap-calendars)"
**Date:** 2026-06-21

## Overview

Add a JMAP-backed implementation of the `internal/calendar` port, the JMAP-native
counterpart to the existing CalDAV adapter (`internal/calendar/caldav`). It lets
go-mailbox-720 front a unified-groupware backend (Stalwart / Cyrus / SOGo) that
speaks **JMAP for Calendars** (`draft-ietf-jmap-calendars`, capability
`urn:ietf:params:jmap:calendars`), presenting it through the same Microsoft Graph
calendar surface as the CalDAV path.

The object model is **go-jscalendar v0.2.0**: a JMAP `CalendarEvent` *is* a
JSCalendar (RFC 8984) `Event`. The library's `jscalendar/jmap` sub-package
(JSCAL-30) supplies the `CalendarEvent ⇄ jscalendar.Event` object mapping; this
adapter owns the **JMAP transport** (the `Calendar/*` and `CalendarEvent/*`
methods + framing) and the `jscalendar.Event ⇄ internal/calendar.Event` glue.

## Goals / scope

Full parity with the CalDAV adapter — all seven port interfaces:
`Backend`, `Writer`, `DeltaReader`, `SchedulingDetector`, `Finder`,
`InstanceReader`, `InstanceWriter`.

## Non-goals

- No change to the `internal/calendar` port types or the Graph layer above it.
- No standalone `/drive` or other surfaces.
- No client-side iTIP: scheduling is delegated to the JMAP server (see below).

## Architecture

New package `internal/calendar/jmap`, mirroring `internal/contacts/jmap`:

| File | Responsibility |
|---|---|
| `jmap.go` | `Dial(sessionURL, accessToken string, *Options) (*Client, error)`, `newClient` test seam, `do` one-call helper, `Close`. Resolves the primary calendars account via `Session.PrimaryAccounts["urn:ietf:params:jmap:calendars"]`. |
| `calendarevent.go` | Custom JMAP method request/response structs + `gojmap.RegisterMethod` registrations for `Calendar/get`, `CalendarEvent/get`, `CalendarEvent/query`, `CalendarEvent/set`, `CalendarEvent/changes`. |
| `event.go` | `jscalendar.Event ⇄ internal/calendar.Event` mapping (hybrid strategy, below). |
| `id.go` | ID handling — pass-through (see ID scheme). |

Transport: existing `git.sr.ht/~rockorager/go-jmap v0.5.3` (as mail/contacts use —
**not** emersion/go-jmap, despite the issue text). Object model:
`github.com/hstern/go-jscalendar v0.2.0` (`jscalendar`, `jscalendar/jmap`,
`jscalendar/ical`).

## Interface → JMAP method mapping

| Port capability | JMAP realization |
|---|---|
| `ListCalendars` | `Calendar/get` (no ids → all) |
| `ListEvents(cal, range)` | `CalendarEvent/query` (filter: `inCalendars`=[cal], `before`/`after` from range) → `CalendarEvent/get`. Masters only (no `expandRecurrences`). |
| `GetEvent(id)` | `CalendarEvent/get` (single id) |
| `CreateEvent` / `UpdateEvent` / `DeleteEvent` | `CalendarEvent/set` `create` / `update` / `destroy` |
| `Delta(cal, token)` | `CalendarEvent/changes` (`sinceState`=token → `newState`, `created`/`updated`/`destroyed`); fetch changed via `CalendarEvent/get`. Synthetic instance ids are excluded by spec. |
| `FindEventByUID(cal, uid)` | `CalendarEvent/query` filter `uid` |
| `ListInstances(eventID, range)` | `CalendarEvent/query` `expandRecurrences:true` + `before`/`after` → server-minted synthetic ids → `CalendarEvent/get` |
| `WriteInstanceOverride(masterID, override)` | `CalendarEvent/set` `update` on the synthetic instance id (server folds into `recurrenceOverrides`) |
| `SupportsServerScheduling` | returns `true` when the session advertises `urn:ietf:params:jmap:calendars` — scheduling is intrinsic to that capability |

### Recurrence — server-side

Unlike CalDAV (which hand-rolls RRULE expansion in `caldav/recurrence.go` via
go-ical's `RecurrenceSet`), JMAP expands server-side: `CalendarEvent/query` with
`expandRecurrences:true` (requires bounded `before`+`after`) returns one opaque
synthetic id per occurrence, which `/get` and `/set` resolve back to base-event +
recurrence-id. So `InstanceReader`/`InstanceWriter` need **no** client-side
expander. (`jscalendar/recur` remains available as a fallback for a backend that
doesn't advertise `expandRecurrences`, but is not used in the primary path.)

### Scheduling — delegated

JMAP groupware performs RFC 6638-style iTIP itself. `Writer` passes
`sendSchedulingMessages:true` on `CalendarEvent/set` so the server emits
REQUEST/REPLY/CANCEL. `SupportsServerScheduling` returns true, so the client-side
iTIP engine (MB720-10) stands down for JMAP backends — the issue's "bonus".

## ID scheme

JMAP ids are already opaque, stable, and account-scoped, so unlike CalDAV's
base64-encoded hrefs we **pass them through**:

- `Calendar.ID` = JMAP Calendar id; `Event.ID` = JMAP CalendarEvent id.
- Occurrences: `Event.ID` = the server's synthetic instance id (pass-through);
  `SeriesMasterID` = `CalendarEvent.baseEventId`; `RecurrenceID` from the
  occurrence's `recurrenceId`.
- `id.go` stays thin: validation/typing only, no encode/decode.

## Mapping strategy — hybrid (direct + ical-for-RRULE)

Map scalar and participant fields **directly** in `event.go`; use
`jscalendar/ical` **only** to convert recurrence between the structured
`jscalendar.RecurrenceRule` and the RFC 5545 RRULE string that
`internal/calendar.RecurrencePattern.RRULE` carries.

| `calendar.Event` field | `jscalendar.Event` source |
|---|---|
| `UID` | `uid` |
| `Subject` | `title` |
| `Start` | `utcStart` (fetched) or `start`+`timeZone` resolved to UTC |
| `End` | `utcEnd` or `start`+`duration` |
| `IsAllDay` | `showWithoutTime` |
| `Location` | first `locations[*].name` |
| `Organizer` | participant whose `roles` includes `owner` |
| `Attendees` | `participants` → `{Name, Email(sendTo/email), Status(participationStatus), ScheduleStatus}` |
| `Body` | `description` (+ `descriptionContentType`) |
| `Status` | `status` |
| `Sequence` | `sequence` |
| `CreatedAt` | `created` |
| `Recurrence.RRULE` | `recurrenceRules` → RRULE string **via `jscalendar/ical`** |
| `Recurrence.ExceptionDates` | `recurrenceOverrides` keys where `excluded:true` |
| `RecurrenceID` | `recurrenceId` (+ `recurrenceIdTimeZone`) |
| `IsOverride` | occurrence present as a stored override vs synthesized |

Participation-status mapping mirrors `caldav/partstat.go`'s neutral vocabulary
("accepted"/"declined"/"tentativelyAccepted"/"notResponded"). Writes invert each
row; RRULE string → `recurrenceRules` also via `jscalendar/ical`.

## Wiring / configuration

Mirror the JMAP mail adapter in `cmd/mailboxd/main.go`:

- New `staticJMAPCalendarProvider{ sessionURL, token string }` implementing
  `server.CalendarProvider`; its `Calendar()` calls `jmap.Dial`.
- Flag `-cal-jmap-session-url`; token from env `MAILBOXD_CALENDAR_JMAP_TOKEN`
  (never a flag — secret).
- Selection: JMAP wins when `-cal-jmap-session-url` is set, else CalDAV when
  `-cal-caldav-url` is set, else calendar ops return 501 (unchanged).

## Error handling

- `do` surfaces a server `MethodError` as a Go error (existing pattern).
- `CalendarEvent/set` inspects `notCreated`/`notUpdated`/`notDestroyed`
  `SetError` and returns a wrapped error (mirror `mail/jmap/write.go`).
- Unknown/recoverable JSCalendar members are preserved by go-jscalendar's
  `Extra` round-trip; the adapter does not reject on unknown properties.

## Testing

- **Unit** (`jmap_test.go`, `event_test.go`): `httptest` JMAP server returning
  canned Session + method responses, injected via `newClient` — the
  `contacts/jmap_test.go` pattern. Cover every interface, the mapping table both
  directions, recurrence master vs expanded instance, and `SetError` paths.
- **Integration** (`stalwart_test.go`, build-tagged): against a real Stalwart
  (speaks JMAP Calendars), mirroring `caldav/stalwart_test.go`. Validates
  `expandRecurrences` and `sendSchedulingMessages` against a live server.

## Risks / open questions

- **`expandRecurrences` support varies** by server; primary path assumes it.
  Fallback to `jscalendar/recur` is possible but deferred unless a target server
  lacks it.
- **`utcStart`/`utcEnd` are not returned by default** — must be requested in
  `CalendarEvent/get` `properties`, or computed from `start`+`timeZone`+`duration`.
- **Lossy fields**: JMAP participant richness collapses to the neutral
  `Attendee`; acceptable for Graph parity (same loss as the CalDAV path).

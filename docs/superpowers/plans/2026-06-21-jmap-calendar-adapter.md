# JMAP Calendar Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a JMAP-backed `internal/calendar` adapter at `internal/calendar/jmap`, at full parity with the existing CalDAV adapter (all 7 port interfaces).

**Architecture:** A `Client` over `git.sr.ht/~rockorager/go-jmap` (the transport, as the mail/contacts adapters use) maps JMAP `Calendar`/`CalendarEvent` objects ⇄ the neutral `internal/calendar` model. `CalendarEvent` objects are JSCalendar (RFC 8984) events handled via `github.com/hstern/go-jscalendar` v0.2.0; recurrence is expanded server-side (`expandRecurrences`) and scheduling delegated server-side (`sendSchedulingMessages`).

**Tech Stack:** Go 1.26; `git.sr.ht/~rockorager/go-jmap v0.5.3`; `github.com/hstern/go-jscalendar v0.2.0` (packages `jscalendar`, `jscalendar/jmap`, `jscalendar/ical`); `net/http/httptest` for unit tests.

## Global Constraints

- JMAP capability URN: `urn:ietf:params:jmap:calendars` (exact).
- Transport library is `git.sr.ht/~rockorager/go-jmap` — NOT emersion/go-jmap.
- Bearer token is sourced from env only at the call site, never a CLI flag.
- IDs are pass-through: `string(gojmap.ID)` ⇄ neutral string IDs; no encode/decode.
- Mirror `internal/contacts/jmap` for structure/conventions; mirror `internal/mail/jmap/write.go` for the set/SetError pattern.
- Follow repo convention: tests alongside source; behavior-focused; use the `commit` agent for commits (the steps below show the message to use).
- Design reference: `docs/superpowers/specs/2026-06-21-jmap-calendar-adapter-design.md`.

---

### Task 1: Package scaffolding — dependency, Client, Dial, do, Close

**Files:**
- Modify: `go.mod`, `go.sum` (add go-jscalendar)
- Create: `internal/calendar/jmap/jmap.go`
- Test: `internal/calendar/jmap/jmap_test.go`

**Interfaces:**
- Produces:
  - `type Client struct{ c *gojmap.Client; accountID gojmap.ID }`
  - `func Dial(sessionURL, accessToken string, o *Options) (*Client, error)`
  - `func newClient(c *gojmap.Client, accountID gojmap.ID) *Client` (test seam)
  - `func (cl *Client) do(ctx context.Context, m gojmap.Method) (any, error)`
  - `func (cl *Client) Close() error`
  - `const calendarsURI gojmap.URI = "urn:ietf:params:jmap:calendars"`
  - `var _ calendar.Backend = (*Client)(nil)` (assertion added now; methods filled in later tasks — keep this commented until Task 5 so the package compiles, OR add a stub `Close` only). To keep the package compiling, add only `Close` here and defer the `Backend` assertion to Task 5.

- [ ] **Step 1: Add the dependency**

Run from the worktree root:
```bash
go get github.com/hstern/go-jscalendar@v0.2.0
go mod tidy
```
Expected: `go.mod` gains `github.com/hstern/go-jscalendar v0.2.0`.

- [ ] **Step 2: Write the failing test**

`internal/calendar/jmap/jmap_test.go`:
```go
package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// sessionJSON is a minimal JMAP Session advertising a primary calendars account.
func sessionJSON(apiURL string) string {
	return `{"capabilities":{"urn:ietf:params:jmap:calendars":{}},` +
		`"accounts":{"acc1":{"name":"u","isPersonal":true,"accountCapabilities":{}}},` +
		`"primaryAccounts":{"urn:ietf:params:jmap:calendars":"acc1"},` +
		`"apiUrl":"` + apiURL + `","downloadUrl":"","uploadUrl":"","eventSourceUrl":"","state":"s"}`
}

// jmapServer spins an httptest server: GET <any> returns the session, POST returns
// the supplied api handler's body.
func jmapServer(t *testing.T, api func(w http.ResponseWriter, body map[string]any)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sessionJSON(srv.URL + "/jmap")))
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		api(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDialResolvesCalendarAccount(t *testing.T) {
	srv := jmapServer(t, func(w http.ResponseWriter, _ map[string]any) {})
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if cl.accountID != gojmap.ID("acc1") {
		t.Fatalf("accountID = %q, want acc1", cl.accountID)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDialNoCalendarsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"capabilities":{},"accounts":{},"primaryAccounts":{},"apiUrl":"x","state":"s"}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := Dial(srv.URL, "tok", nil); err == nil {
		t.Fatal("expected error when no primary calendars account")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestDial -v`
Expected: FAIL (build error — `Dial` undefined).

- [ ] **Step 4: Write the implementation**

`internal/calendar/jmap/jmap.go` (adapted from `internal/contacts/jmap/jmap.go`):
```go
// Package jmap implements the internal/calendar port over JMAP for Calendars
// (draft-ietf-jmap-calendars, capability urn:ietf:params:jmap:calendars). It is
// the JMAP-native counterpart of the CalDAV adapter; CalendarEvent objects are
// JSCalendar (RFC 8984) events handled via github.com/hstern/go-jscalendar.
package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

const calendarsURI gojmap.URI = "urn:ietf:params:jmap:calendars"

// Options configures the JMAP calendars connection.
type Options struct {
	// SessionEndpoint overrides the JMAP Session resource URL. When empty, Dial
	// uses the URL passed to Dial as the session endpoint.
	SessionEndpoint string
}

// Client is a JMAP-backed calendar.Backend over one authenticated session and
// calendars account.
type Client struct {
	c         *gojmap.Client
	accountID gojmap.ID
}

// Dial authenticates to the JMAP server, fetches the Session, and resolves the
// primary calendars account. The access token is the operator's JMAP credential,
// always sourced from an environment secret at the call site.
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
	accountID, ok := c.Session.PrimaryAccounts[calendarsURI]
	if !ok || accountID == "" {
		return nil, fmt.Errorf("jmap: session advertises no primary calendars account (%s)", calendarsURI)
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
```
> NOTE: confirm the exact `Session` field name for primary accounts (`PrimaryAccounts`) and the `Client` API against `internal/contacts/jmap/jmap.go` — copy whatever that file uses verbatim, since it is known-good against this go-jmap version.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/calendar/jmap/ -run TestDial -v`
Expected: PASS (both).

- [ ] **Step 6: Commit**

Message: `Add JMAP calendar adapter scaffolding (Dial/do/Close) (MB720-20)`

---

### Task 2: JMAP method request/response types

**Files:**
- Create: `internal/calendar/jmap/calendarevent.go`
- Test: `internal/calendar/jmap/calendarevent_test.go`

**Interfaces:**
- Consumes: `calendarsURI` (Task 1).
- Produces (all implement `gojmap.Method` via `Name()`/`Requires()`; responses registered in `init`):
  - `calendarGet{Account gojmap.ID; IDs []gojmap.ID}` → `calendarGetResponse{Account gojmap.ID; State string; List []*jmapcal.Calendar}` — name `Calendar/get`
  - `eventGet{Account gojmap.ID; IDs []gojmap.ID; Properties []string}` → `eventGetResponse{Account gojmap.ID; State string; List []*jscal.CalendarEvent; NotFound []gojmap.ID}` — name `CalendarEvent/get`
  - `eventQuery{Account gojmap.ID; Filter *eventFilter; ExpandRecurrences bool}` → `eventQueryResponse{Account gojmap.ID; IDs []gojmap.ID}` — name `CalendarEvent/query`
  - `eventChanges{Account gojmap.ID; SinceState string; MaxChanges uint64}` → `eventChangesResponse{Account gojmap.ID; OldState, NewState string; HasMoreChanges bool; Created, Updated []gojmap.ID; Destroyed []gojmap.ID}` — name `CalendarEvent/changes`
  - `eventSet{Account gojmap.ID; Create map[gojmap.ID]*jscal.CalendarEvent; Update map[gojmap.ID]gojmap.Patch; Destroy []gojmap.ID; SendSchedulingMessages bool}` → `eventSetResponse{Account gojmap.ID; OldState, NewState string; Created, Updated map[gojmap.ID]*jscal.CalendarEvent; Destroyed []gojmap.ID; NotCreated, NotUpdated, NotDestroyed map[gojmap.ID]*gojmap.SetError}` — name `CalendarEvent/set`
  - `eventFilter{InCalendars []gojmap.ID; After, Before string; UID string}` (a JMAP FilterCondition; property names confirmed against the draft/Stalwart at implementation)
  - where `jscal` = `github.com/hstern/go-jscalendar/jmap`, and `jmapcal.Calendar` is a local struct `type jmapCalendar struct{ ID gojmap.ID; Name string; Description string }` (the `Calendar` object subset we read).

- [ ] **Step 1: Write the failing test**

`internal/calendar/jmap/calendarevent_test.go`:
```go
package jmap

import (
	"encoding/json"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

func TestEventGetMethodShape(t *testing.T) {
	m := &eventGet{Account: "acc1", IDs: []gojmap.ID{"e1"}}
	if m.Name() != "CalendarEvent/get" {
		t.Fatalf("Name = %q", m.Name())
	}
	if got := m.Requires(); len(got) != 1 || got[0] != calendarsURI {
		t.Fatalf("Requires = %v", got)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"accountId":"acc1","ids":["e1"]}` {
		t.Fatalf("json = %s", b)
	}
}

func TestEventQueryExpandRecurrencesMarshals(t *testing.T) {
	m := &eventQuery{Account: "acc1", Filter: &eventFilter{InCalendars: []gojmap.ID{"c1"}, After: "2026-01-01T00:00:00Z", Before: "2026-02-01T00:00:00Z"}, ExpandRecurrences: true}
	b, _ := json.Marshal(m)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["expandRecurrences"] != true {
		t.Fatalf("expandRecurrences missing: %s", b)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestEvent -v`
Expected: FAIL (build error — types undefined).

- [ ] **Step 3: Write the implementation**

`internal/calendar/jmap/calendarevent.go`:
```go
package jmap

import (
	gojmap "git.sr.ht/~rockorager/go-jmap"
	jscal "github.com/hstern/go-jscalendar/jmap"
)

func init() {
	gojmap.RegisterMethod("Calendar/get", func() gojmap.MethodResponse { return &calendarGetResponse{} })
	gojmap.RegisterMethod("CalendarEvent/get", func() gojmap.MethodResponse { return &eventGetResponse{} })
	gojmap.RegisterMethod("CalendarEvent/query", func() gojmap.MethodResponse { return &eventQueryResponse{} })
	gojmap.RegisterMethod("CalendarEvent/changes", func() gojmap.MethodResponse { return &eventChangesResponse{} })
	gojmap.RegisterMethod("CalendarEvent/set", func() gojmap.MethodResponse { return &eventSetResponse{} })
}

// --- Calendar/get ---

type jmapCalendar struct {
	ID          gojmap.ID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

type calendarGet struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids,omitempty"`
}

func (m *calendarGet) Name() string             { return "Calendar/get" }
func (m *calendarGet) Requires() []gojmap.URI   { return []gojmap.URI{calendarsURI} }

type calendarGetResponse struct {
	Account gojmap.ID       `json:"accountId"`
	State   string          `json:"state"`
	List    []*jmapCalendar `json:"list"`
}

// --- CalendarEvent/get ---

type eventGet struct {
	Account    gojmap.ID   `json:"accountId"`
	IDs        []gojmap.ID `json:"ids,omitempty"`
	Properties []string    `json:"properties,omitempty"`
}

func (m *eventGet) Name() string           { return "CalendarEvent/get" }
func (m *eventGet) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventGetResponse struct {
	Account  gojmap.ID             `json:"accountId"`
	State    string                `json:"state"`
	List     []*jscal.CalendarEvent `json:"list"`
	NotFound []gojmap.ID           `json:"notFound"`
}

// --- CalendarEvent/query ---

type eventFilter struct {
	InCalendars []gojmap.ID `json:"inCalendars,omitempty"`
	After       string      `json:"after,omitempty"`
	Before      string      `json:"before,omitempty"`
	UID         string      `json:"uid,omitempty"`
}

type eventQuery struct {
	Account           gojmap.ID    `json:"accountId"`
	Filter            *eventFilter `json:"filter,omitempty"`
	ExpandRecurrences bool         `json:"expandRecurrences,omitempty"`
}

func (m *eventQuery) Name() string           { return "CalendarEvent/query" }
func (m *eventQuery) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventQueryResponse struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids"`
}

// --- CalendarEvent/changes ---

type eventChanges struct {
	Account    gojmap.ID `json:"accountId"`
	SinceState string    `json:"sinceState"`
	MaxChanges uint64    `json:"maxChanges,omitempty"`
}

func (m *eventChanges) Name() string           { return "CalendarEvent/changes" }
func (m *eventChanges) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventChangesResponse struct {
	Account        gojmap.ID   `json:"accountId"`
	OldState       string      `json:"oldState"`
	NewState       string      `json:"newState"`
	HasMoreChanges bool        `json:"hasMoreChanges"`
	Created        []gojmap.ID `json:"created"`
	Updated        []gojmap.ID `json:"updated"`
	Destroyed      []gojmap.ID `json:"destroyed"`
}

// --- CalendarEvent/set ---

type eventSet struct {
	Account                gojmap.ID                        `json:"accountId"`
	Create                 map[gojmap.ID]*jscal.CalendarEvent `json:"create,omitempty"`
	Update                 map[gojmap.ID]gojmap.Patch        `json:"update,omitempty"`
	Destroy                []gojmap.ID                       `json:"destroy,omitempty"`
	SendSchedulingMessages bool                              `json:"sendSchedulingMessages,omitempty"`
}

func (m *eventSet) Name() string           { return "CalendarEvent/set" }
func (m *eventSet) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventSetResponse struct {
	Account      gojmap.ID                          `json:"accountId"`
	OldState     string                             `json:"oldState"`
	NewState     string                             `json:"newState"`
	Created      map[gojmap.ID]*jscal.CalendarEvent `json:"created"`
	Updated      map[gojmap.ID]*jscal.CalendarEvent `json:"updated"`
	Destroyed    []gojmap.ID                        `json:"destroyed"`
	NotCreated   map[gojmap.ID]*gojmap.SetError     `json:"notCreated"`
	NotUpdated   map[gojmap.ID]*gojmap.SetError     `json:"notUpdated"`
	NotDestroyed map[gojmap.ID]*gojmap.SetError     `json:"notDestroyed"`
}
```
> NOTE: verify `gojmap.Patch` and `gojmap.SetError` field names against `internal/mail/jmap/write.go`. If `RegisterMethod`/`MethodResponse`/`Requires` differ from this go-jmap version, copy the exact pattern from `internal/contacts/jmap/contactcard.go`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/calendar/jmap/ -run TestEvent -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Message: `Add JMAP Calendar/CalendarEvent method types (MB720-20)`

---

### Task 3: Read mapping — jscalendar.Event → calendar.Event

**Files:**
- Create: `internal/calendar/jmap/event.go`
- Test: `internal/calendar/jmap/event_test.go`

**Interfaces:**
- Consumes: `jscal.CalendarEvent` (Task 2), `github.com/hstern/go-jscalendar` core types.
- Produces:
  - `func toCalendarEvent(ce *jscal.CalendarEvent) (calendar.Event, error)` — maps a JMAP CalendarEvent to the neutral model. Sets `ID` from `ce.ID`, `CalendarID` from the first key of `ce.CalendarIDs`, `SeriesMasterID` from `ce.BaseEventID`, and the JSCalendar fields per the design's mapping table.
  - `func rruleFromRules(start *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId, rules []jscalendar.RecurrenceRule) (string, error)` — converts structured rules to an RFC 5545 RRULE string via `jscalendar/ical` (builds a throwaway Event, `ToICal`, reads the `RRULE` property value).
  - `func partStatToStatus(s string) string` — JSCalendar participationStatus → neutral ("accepted"/"declined"/"tentativelyAccepted"/"notResponded"); mirror `caldav/partstat.go` vocabulary.

- [ ] **Step 1: Write the failing test**

`internal/calendar/jmap/event_test.go`:
```go
package jmap

import (
	"testing"

	"github.com/hstern/go-jscalendar"
	jscal "github.com/hstern/go-jscalendar/jmap"
)

func TestToCalendarEventScalars(t *testing.T) {
	ev := &jscalendar.Event{
		UID:         "uid-1",
		Title:       "Standup",
		Status:      "confirmed",
		Description: "daily",
		Sequence:    2,
	}
	ce := jscal.FromEvent(ev)
	ce.ID = "e1"
	ce.CalendarIDs = map[jscalendar.Id]bool{"c1": true}

	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}
	if got.ID != "e1" || got.CalendarID != "c1" {
		t.Fatalf("ids: %+v", got)
	}
	if got.UID != "uid-1" || got.Subject != "Standup" || got.Status != "confirmed" {
		t.Fatalf("fields: %+v", got)
	}
	if got.Body.Content != "daily" || got.Sequence != 2 {
		t.Fatalf("body/seq: %+v", got)
	}
}

func TestToCalendarEventRecurrenceRRULE(t *testing.T) {
	ev := &jscalendar.Event{
		UID:   "uid-2",
		Title: "Weekly",
		Start: &jscalendar.LocalDateTime{Year: 2026, Month: 1, Day: 5, Hour: 9},
		RecurrenceRules: []jscalendar.RecurrenceRule{
			{Frequency: "weekly", ByDay: []jscalendar.NDay{{Day: "mo"}}},
		},
	}
	ce := jscal.FromEvent(ev)
	ce.ID = "e2"
	got, err := toCalendarEvent(ce)
	if err != nil {
		t.Fatalf("toCalendarEvent: %v", err)
	}
	if got.Recurrence == nil || got.Recurrence.RRULE == "" {
		t.Fatalf("recurrence not mapped: %+v", got.Recurrence)
	}
	// RRULE string should carry the weekly Monday rule.
	if want := "FREQ=WEEKLY"; !contains(got.Recurrence.RRULE, want) {
		t.Fatalf("RRULE %q missing %q", got.Recurrence.RRULE, want)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestToCalendarEvent -v`
Expected: FAIL (build error — `toCalendarEvent` undefined).

- [ ] **Step 3: Write the implementation**

`internal/calendar/jmap/event.go` — implement `toCalendarEvent` per the design's mapping table. Key points:
- `ID = string(ce.ID)`; `CalendarID` = first key of `ce.CalendarIDs`; `SeriesMasterID` = `string(*ce.BaseEventID)` when non-nil.
- `UID`, `Subject`←`Title`, `Status`, `Sequence`, `Body{ContentType: descriptionContentType or "text", Content: Description}`, `CreatedAt`←`Created`.
- `Start`/`End`: prefer `ce.UTCStart`/`ce.UTCEnd`; else resolve `Start`+`TimeZone` (+`Duration` for End). Use a helper `localToUTC(*jscalendar.LocalDateTime, jscalendar.TimeZoneId) time.Time`.
- `IsAllDay` ← `ShowWithoutTime`.
- `Location` ← first `Locations[*].Name` (stable iteration: pick deterministically, e.g. sort keys).
- `Organizer`/`Attendees` ← `Participants` (role `owner` → Organizer; all → Attendees with `partStatToStatus`).
- `Recurrence` ← when `len(RecurrenceRules) > 0`: `RecurrencePattern{RRULE: rruleFromRules(...), ExceptionDates: <override keys with excluded:true>}`.
- `RecurrenceID` ← `localToUTC(RecurrenceID, RecurrenceIDTimeZone)`.

Implement `rruleFromRules` via the ical bridge:
```go
func rruleFromRules(start *jscalendar.LocalDateTime, tz jscalendar.TimeZoneId, rules []jscalendar.RecurrenceRule) (string, error) {
	tmp := &jscalendar.Event{UID: "x", Start: start, TimeZone: tz, RecurrenceRules: rules}
	cal, err := ical.ToICal(tmp)
	if err != nil {
		return "", fmt.Errorf("jmap: rrule encode: %w", err)
	}
	for _, comp := range cal.Children {
		if comp.Name != "VEVENT" {
			continue
		}
		if p := comp.Props.Get("RRULE"); p != nil {
			return p.Value, nil
		}
	}
	return "", nil
}
```
(import `ical "github.com/hstern/go-jscalendar/ical"`; confirm the go-ical `Calendar.Children`/`Props.Get` accessors against `internal/calendar/caldav` usage.)

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/calendar/jmap/ -run TestToCalendarEvent -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Message: `Add JMAP CalendarEvent -> calendar.Event mapping (MB720-20)`

---

### Task 4: Write mapping — calendar.Event → jscal.CalendarEvent

**Files:**
- Modify: `internal/calendar/jmap/event.go`
- Test: `internal/calendar/jmap/event_test.go`

**Interfaces:**
- Produces:
  - `func fromCalendarEvent(e calendar.Event) (*jscal.CalendarEvent, error)` — inverse of `toCalendarEvent`. Sets `CalendarIDs` from `e.CalendarID`, the JSCalendar fields, and recurrence via `rulesFromRRULE`.
  - `func rulesFromRRULE(rrule string) ([]jscalendar.RecurrenceRule, error)` — RFC 5545 RRULE string → structured rules via `jscalendar/ical` (`FromICal` on a throwaway VEVENT carrying the RRULE).

- [ ] **Step 1: Write the failing test**

Add to `event_test.go`:
```go
func TestFromCalendarEventRoundTrip(t *testing.T) {
	in := calendar.Event{
		UID:        "uid-3",
		CalendarID: "c1",
		Subject:    "Review",
		Status:     "tentative",
		Body:       calendar.Body{ContentType: "text", Content: "notes"},
	}
	ce, err := fromCalendarEvent(in)
	if err != nil {
		t.Fatalf("fromCalendarEvent: %v", err)
	}
	if ce.UID != "uid-3" || ce.Title != "Review" || ce.Status != "tentative" {
		t.Fatalf("fields: %+v", ce.Event)
	}
	if !ce.CalendarIDs["c1"] {
		t.Fatalf("calendarIds: %+v", ce.CalendarIDs)
	}
}
```
(add the `calendar "github.com/hstern/go-mailbox-720/internal/calendar"` import.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestFromCalendarEvent -v`
Expected: FAIL (`fromCalendarEvent` undefined).

- [ ] **Step 3: Write the implementation**

Implement `fromCalendarEvent` (inverse mapping) and `rulesFromRRULE` (mirror `rruleFromRules` but via `ical.FromICal`). Map each row of the design table in reverse; set `ce.CalendarIDs = map[jscalendar.Id]bool{jscalendar.Id(e.CalendarID): true}` when `e.CalendarID != ""`; build `Participants` from `Organizer`+`Attendees` with the inverse status map.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/calendar/jmap/ -run "TestFromCalendarEvent|TestToCalendarEvent" -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Message: `Add calendar.Event -> JMAP CalendarEvent mapping (MB720-20)`

---

### Task 5: Backend.ListCalendars

**Files:**
- Modify: `internal/calendar/jmap/jmap.go` (add `var _ calendar.Backend = (*Client)(nil)` once all 3 Backend methods exist across Tasks 5-7; add it in Task 7)
- Create: `internal/calendar/jmap/backend.go`
- Test: `internal/calendar/jmap/backend_test.go`

**Interfaces:**
- Produces: `func (cl *Client) ListCalendars(ctx context.Context) ([]calendar.Calendar, error)` — `Calendar/get` (no ids) → map each `*jmapCalendar` to `calendar.Calendar{ID,Name,Description}`.

- [ ] **Step 1: Write the failing test**

`internal/calendar/jmap/backend_test.go`:
```go
package jmap

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// dialTest returns a Client wired to a jmapServer with the given POST handler.
func dialTest(t *testing.T, api func(w http.ResponseWriter, body map[string]any)) *Client {
	t.Helper()
	srv := jmapServer(t, api)
	cl, err := Dial(srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return cl
}

// respond writes a JMAP method-response envelope wrapping one invocation.
func respond(w http.ResponseWriter, method string, args any) {
	a, _ := json.Marshal(args)
	out := map[string]any{
		"methodResponses": []any{[]any{method, json.RawMessage(a), "c0"}},
		"sessionState":    "s",
	}
	_ = json.NewEncoder(w).Encode(out)
}

func TestListCalendars(t *testing.T) {
	cl := dialTest(t, func(w http.ResponseWriter, _ map[string]any) {
		respond(w, "Calendar/get", map[string]any{
			"accountId": "acc1", "state": "1",
			"list": []map[string]any{{"id": "c1", "name": "Personal", "description": "mine"}},
		})
	})
	cals, err := cl.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 || cals[0].ID != "c1" || cals[0].Name != "Personal" {
		t.Fatalf("cals = %+v", cals)
	}
	_ = gojmap.ID("")
}
```
> NOTE: confirm the response envelope key (`methodResponses`) and invocation tuple shape against go-jmap's decoder — adjust `respond` to whatever `cl.c.Do` expects. Cross-check with how `internal/contacts/jmap/jmap_test.go` frames responses and copy that exactly.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestListCalendars -v`
Expected: FAIL (`ListCalendars` undefined).

- [ ] **Step 3: Write the implementation**

`internal/calendar/jmap/backend.go`:
```go
package jmap

import (
	"context"
	"fmt"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func (cl *Client) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	args, err := cl.do(ctx, &calendarGet{Account: cl.accountID})
	if err != nil {
		return nil, err
	}
	resp, ok := args.(*calendarGetResponse)
	if !ok {
		return nil, fmt.Errorf("jmap: unexpected response %T for Calendar/get", args)
	}
	out := make([]calendar.Calendar, 0, len(resp.List))
	for _, c := range resp.List {
		out = append(out, calendar.Calendar{ID: string(c.ID), Name: c.Name, Description: c.Description})
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/calendar/jmap/ -run TestListCalendars -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Message: `Add JMAP ListCalendars (MB720-20)`

---

### Task 6: Backend.ListEvents

**Files:**
- Modify: `internal/calendar/jmap/backend.go`
- Test: `internal/calendar/jmap/backend_test.go`

**Interfaces:**
- Consumes: `toCalendarEvent` (Task 3), `eventQuery`/`eventGet` (Task 2).
- Produces: `func (cl *Client) ListEvents(ctx context.Context, calendarID string, r calendar.Range) ([]calendar.Event, error)` — `CalendarEvent/query` (filter `inCalendars=[calendarID]`, `after`/`before` from `r` when non-zero) → `CalendarEvent/get` of the returned ids → `toCalendarEvent` each. Empty ids → nil.

- [ ] **Step 1: Write the failing test**

Add to `backend_test.go` a `TestListEvents` that serves a two-call request: a `CalendarEvent/query` returning `{"ids":["e1"]}` then a `CalendarEvent/get` returning one event with `id:"e1"`, `uid:"u1"`, `title:"M"`. (Because `do` issues one method per request, ListEvents will make two round-trips; have the handler switch on the method name in `body["methodCalls"]`.) Assert one event back with `ID=="e1"`, `Subject=="M"`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/calendar/jmap/ -run TestListEvents -v`
Expected: FAIL.

- [ ] **Step 3: Write the implementation**

Add `ListEvents`: build `eventFilter` with `InCalendars: []gojmap.ID{gojmap.ID(calendarID)}` and RFC3339 `After`/`Before` from `r.Start`/`r.End` when non-zero; `do` the query; if `len(ids)==0` return nil; `do` an `eventGet{IDs: ids, Properties: []string{... include "utcStart","utcEnd"}}`; map each with `toCalendarEvent`. Add a `getEvents(ctx, ids)` private helper (reused by Tasks 7/9/12).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/calendar/jmap/ -run TestListEvents -v`
Expected: PASS.

- [ ] **Step 5: Commit**

Message: `Add JMAP ListEvents (query+get over a time range) (MB720-20)`

---

### Task 7: Backend.GetEvent + Backend assertion

**Files:**
- Modify: `internal/calendar/jmap/backend.go`, `internal/calendar/jmap/jmap.go`
- Test: `internal/calendar/jmap/backend_test.go`

**Interfaces:**
- Produces: `func (cl *Client) GetEvent(ctx context.Context, id string) (calendar.Event, error)` — `CalendarEvent/get` single id → `toCalendarEvent`; not-found → error. Also add `var _ calendar.Backend = (*Client)(nil)` to `jmap.go`.

- [ ] **Step 1: Write the failing test** — `TestGetEvent` serves `CalendarEvent/get` for `["e1"]` returning one event; asserts `ID=="e1"`. Add `TestGetEventNotFound` returning empty `list` + `notFound:["e1"]`; asserts error.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestGetEvent -v` → FAIL.
- [ ] **Step 3: Write the implementation** — `GetEvent` via the `getEvents` helper; error when the list is empty. Add the `var _ calendar.Backend = (*Client)(nil)` assertion.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestGetEvent -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP GetEvent + Backend assertion (MB720-20)`

---

### Task 8: Writer (Create/Update/Delete)

**Files:**
- Create: `internal/calendar/jmap/write.go`
- Test: `internal/calendar/jmap/write_test.go`

**Interfaces:**
- Consumes: `fromCalendarEvent` (Task 4), `eventSet` (Task 2).
- Produces:
  - `func (cl *Client) CreateEvent(ctx context.Context, calendarID string, e calendar.Event) (calendar.Event, error)`
  - `func (cl *Client) UpdateEvent(ctx context.Context, e calendar.Event) (calendar.Event, error)`
  - `func (cl *Client) DeleteEvent(ctx context.Context, id string) error`
  - `func setErrorString(se *gojmap.SetError) string`
  - `var _ calendar.Writer = (*Client)(nil)`

- [ ] **Step 1: Write the failing test** — `TestCreateEvent` serves `CalendarEvent/set` returning `created:{"new":{"id":"e9", ...}}`; asserts returned event `ID=="e9"`. `TestCreateEventRejected` returns `notCreated:{"new":{"type":"invalidProperties"}}`; asserts error. `TestDeleteEvent` returns `destroyed:["e1"]`; asserts nil.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run "TestCreateEvent|TestDeleteEvent" -v` → FAIL.
- [ ] **Step 3: Write the implementation** — mirror `internal/mail/jmap/write.go`:
  - Create: `fromCalendarEvent(e)`, set `CalendarIDs` from `calendarID`, send `eventSet{Create: {"new": ce}, SendSchedulingMessages: true}`; on `NotCreated["new"]` return `setErrorString`; else `toCalendarEvent(resp.Created["new"])`.
  - Update: send `eventSet{Update: {gojmap.ID(e.ID): patchFrom(ce)}}` where `patchFrom` builds a `gojmap.Patch` of the full object (or use a whole-object replace patch keyed by `""`/per-property — confirm against mail/jmap). Inspect `NotUpdated`.
  - Delete: `eventSet{Destroy: []gojmap.ID{gojmap.ID(id)}}`; inspect `NotDestroyed`.
  - Add `var _ calendar.Writer = (*Client)(nil)`.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run "TestCreateEvent|TestDeleteEvent" -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP Writer (CalendarEvent/set create/update/destroy) (MB720-20)`

---

### Task 9: DeltaReader.Delta

**Files:**
- Create: `internal/calendar/jmap/delta.go`
- Test: `internal/calendar/jmap/delta_test.go`

**Interfaces:**
- Produces: `func (cl *Client) Delta(ctx context.Context, calendarID string, token string) (changed []calendar.Event, removed []string, next string, err error)` — `CalendarEvent/changes{SinceState: token}` → fetch `Created`+`Updated` via `getEvents` (filter to `calendarID` via the events' `CalendarIDs`), map `Destroyed` ids into `removed`, return `NewState` as `next`. `var _ calendar.DeltaReader = (*Client)(nil)`.

- [ ] **Step 1: Write the failing test** — `TestDelta` serves `CalendarEvent/changes` returning `{oldState, newState:"2", created:["e1"], updated:[], destroyed:["e0"]}` then `CalendarEvent/get` for `["e1"]`. Asserts one `changed` event, `removed==["e0"]`, `next=="2"`.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestDelta -v` → FAIL.
- [ ] **Step 3: Write the implementation** — per the Produces block; skip `getEvents` when there are no created/updated ids.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestDelta -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP DeltaReader (CalendarEvent/changes) (MB720-20)`

---

### Task 10: Finder.FindEventByUID

**Files:**
- Modify: `internal/calendar/jmap/backend.go`
- Test: `internal/calendar/jmap/backend_test.go`

**Interfaces:**
- Produces: `func (cl *Client) FindEventByUID(ctx context.Context, calendarID, uid string) (calendar.Event, bool, error)` — `CalendarEvent/query{Filter:{InCalendars:[calendarID], UID:uid}}` → if no ids return `(zero,false,nil)` → else `getEvents` first id → `(event,true,nil)`. `var _ calendar.Finder = (*Client)(nil)`.

- [ ] **Step 1: Write the failing test** — `TestFindEventByUIDFound` (query→`["e1"]`, get→event) asserts `(found==true, ID=="e1")`; `TestFindEventByUIDMissing` (query→`[]`) asserts `found==false, err==nil`.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestFindEventByUID -v` → FAIL.
- [ ] **Step 3: Write the implementation** — per Produces.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestFindEventByUID -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP Finder (FindEventByUID) (MB720-20)`

---

### Task 11: SchedulingDetector.SupportsServerScheduling

**Files:**
- Create: `internal/calendar/jmap/scheduling.go`
- Test: `internal/calendar/jmap/scheduling_test.go`

**Interfaces:**
- Produces: `func (cl *Client) SupportsServerScheduling(ctx context.Context) (bool, error)` — returns `true` iff `cl.c.Session.Capabilities` contains `calendarsURI` (the calendars capability is the scheduling capability). `var _ calendar.SchedulingDetector = (*Client)(nil)`.

- [ ] **Step 1: Write the failing test** — `TestSupportsServerScheduling`: a Dial'd client (session advertises calendars) returns `(true,nil)`. (Reuse `dialTest`; no extra round-trip.)
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestSupportsServerScheduling -v` → FAIL.
- [ ] **Step 3: Write the implementation** — read `cl.c.Session.Capabilities` (confirm the field name/shape against go-jmap `Session`); membership of `calendarsURI`.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestSupportsServerScheduling -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP SchedulingDetector (MB720-20)`

---

### Task 12: InstanceReader.ListInstances

**Files:**
- Create: `internal/calendar/jmap/recurrence.go`
- Test: `internal/calendar/jmap/recurrence_test.go`

**Interfaces:**
- Produces: `func (cl *Client) ListInstances(ctx context.Context, eventID string, r calendar.Range) ([]calendar.Event, error)` — requires bounded `r` (error if `r.Start`/`r.End` zero); `CalendarEvent/query{Filter:{After,Before}, ExpandRecurrences:true}` (the filter need not constrain by id when the server scopes to the account; if the draft/Stalwart supports a per-base-event filter, add it — confirm at implementation) → `getEvents(ids)` → each carries `recurrenceId`+`baseEventId`, so `toCalendarEvent` yields `RecurrenceID`/`SeriesMasterID` populated; keep only those whose `SeriesMasterID == eventID`. `var _ calendar.InstanceReader = (*Client)(nil)`.

- [ ] **Step 1: Write the failing test** — `TestListInstances` serves `CalendarEvent/query` (expandRecurrences) → `["e1_i0","e1_i1"]` then `CalendarEvent/get` returning two events each with `baseEventId:"e1"` and distinct `recurrenceId`. Asserts 2 instances, each `SeriesMasterID=="e1"`, `!RecurrenceID.IsZero()`. Add `TestListInstancesUnbounded` asserting an error when `r` is zero.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestListInstances -v` → FAIL.
- [ ] **Step 3: Write the implementation** — per Produces; RFC3339 bounds; filter by `SeriesMasterID`.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestListInstances -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP InstanceReader (expandRecurrences) (MB720-20)`

---

### Task 13: InstanceWriter.WriteInstanceOverride

**Files:**
- Modify: `internal/calendar/jmap/write.go`
- Test: `internal/calendar/jmap/write_test.go`

**Interfaces:**
- Produces: `func (cl *Client) WriteInstanceOverride(ctx context.Context, masterID string, override calendar.Event) (calendar.Event, error)` — requires `override.RecurrenceID` non-zero; resolve the synthetic instance id (when `override.ID` is already a synthetic id use it; otherwise the implementer must derive it — for the first cut, require the caller pass the synthetic id in `override.ID`, mirroring how `GetEvent`/`ListInstances` mint it, and document that). Send `eventSet{Update:{instanceID: patch}, SendSchedulingMessages:true}`; map `resp.Updated[instanceID]`. `var _ calendar.InstanceWriter = (*Client)(nil)`.

- [ ] **Step 1: Write the failing test** — `TestWriteInstanceOverride` serves `CalendarEvent/set` returning `updated:{"e1_i0":{...}}`; asserts returned event `IsOverride==true`. `TestWriteInstanceOverrideNoRecurrenceID` asserts error when `RecurrenceID` is zero.
- [ ] **Step 2: Run to verify it fails** — `go test ./internal/calendar/jmap/ -run TestWriteInstanceOverride -v` → FAIL.
- [ ] **Step 3: Write the implementation** — per Produces; set `IsOverride=true` on the returned event.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/calendar/jmap/ -run TestWriteInstanceOverride -v` → PASS.
- [ ] **Step 5: Commit** — `Add JMAP InstanceWriter (WriteInstanceOverride) (MB720-20)`

---

### Task 14: Wiring in cmd/mailboxd

**Files:**
- Modify: `cmd/mailboxd/main.go`
- Test: `go build ./...` + `go vet ./...` (wiring is exercised end-to-end by Task 15)

**Interfaces:**
- Consumes: `jmap.Dial` (Task 1), `server.CalendarProvider`.
- Produces: `staticJMAPCalendarProvider{sessionURL, token string}` with `Calendar(ctx) (calendar.Backend, error)` returning `jmap.Dial(p.sessionURL, p.token, nil)`; CLI flag `-cal-jmap-session-url`; env `MAILBOXD_CALENDAR_JMAP_TOKEN`; selection: JMAP when the flag is set, else CalDAV, else 501.

- [ ] **Step 1: Read the existing CalDAV wiring** — `cmd/mailboxd/main.go` around the `staticCalDAVProvider` definition and the `calProvider` selection block (and how the JMAP *mail* adapter is wired, for the exact flag/env idiom).
- [ ] **Step 2: Add the provider + flag + selection**
```go
// near other flags
jmapCalSession := flag.String("cal-jmap-session-url", "", "JMAP session URL for calendars (token from MAILBOXD_CALENDAR_JMAP_TOKEN)")

// provider type
type staticJMAPCalendarProvider struct{ sessionURL, token string }

func (p staticJMAPCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return jmap.Dial(p.sessionURL, p.token, nil)
}

// selection (JMAP wins, then CalDAV, else nil → 501)
var calProvider server.CalendarProvider
if *jmapCalSession != "" {
	calProvider = staticJMAPCalendarProvider{sessionURL: *jmapCalSession, token: os.Getenv("MAILBOXD_CALENDAR_JMAP_TOKEN")}
} else if *caldavURL != "" {
	calProvider = staticCalDAVProvider{url: *caldavURL, username: *caldavUser, password: os.Getenv("MAILBOXD_CALDAV_PASSWORD")}
}
```
(import `"github.com/hstern/go-mailbox-720/internal/calendar/jmap"`.)
- [ ] **Step 3: Build & vet** — `go build ./... && go vet ./...` → no errors.
- [ ] **Step 4: Commit** — `Wire JMAP calendar provider into mailboxd (MB720-20)`

---

### Task 15: Stalwart integration test

**Files:**
- Create: `internal/calendar/jmap/stalwart_test.go` (build tag, mirroring `internal/calendar/caldav/stalwart_test.go`)

**Interfaces:**
- Consumes: the full `Client`.

- [ ] **Step 1: Read `internal/calendar/caldav/stalwart_test.go`** for the build-tag convention, env-var gating (server URL/credentials), and skip-when-unset pattern.
- [ ] **Step 2: Write the integration test** behind the same build tag: `Dial` against a live Stalwart (URL/token from env, `t.Skip` when unset); exercise `ListCalendars` → `CreateEvent` (with an RRULE) → `ListEvents` → `ListInstances` (bounded range, assert ≥2 occurrences) → `WriteInstanceOverride` → `Delta` → `DeleteEvent`. Assert round-trip fidelity of subject/start/recurrence.
- [ ] **Step 3: Run (gated)** — `go test -tags=integration ./internal/calendar/jmap/ -run TestStalwart -v` when a Stalwart is configured; otherwise confirm it skips cleanly.
- [ ] **Step 4: Commit** — `Add Stalwart integration test for JMAP calendar adapter (MB720-20)`

---

## Final verification

- [ ] `go test ./internal/calendar/jmap/...` — all unit tests pass.
- [ ] `go build ./... && go vet ./...` — clean.
- [ ] Dispatch `code-reviewer` over the full diff before opening a PR.

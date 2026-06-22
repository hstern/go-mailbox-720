package calendar

import (
	"testing"
	"time"

	"github.com/hstern/go-jscalendar"
)

// StartGraph/EndGraph must preserve the event's named IANA zone into Graph's
// dateTimeTimeZone form rather than collapsing to UTC — the fidelity the bespoke
// pivot lost (MB720-49).
func TestStartEndGraphPreservesNamedZone(t *testing.T) {
	e := Event{}
	if err := e.SetStartGraph("2026-06-19T13:00:00.0000000", "America/New_York"); err != nil {
		t.Fatalf("SetStartGraph: %v", err)
	}
	if err := e.SetEndGraph("2026-06-19T14:30:00.0000000"); err != nil {
		t.Fatalf("SetEndGraph: %v", err)
	}

	sd, stz, ok := e.StartGraph()
	if !ok || sd != "2026-06-19T13:00:00.0000000" || stz != "America/New_York" {
		t.Errorf("StartGraph = (%q,%q,%v), want (2026-06-19T13:00:00.0000000, America/New_York, true)", sd, stz, ok)
	}
	ed, etz, ok := e.EndGraph()
	if !ok || ed != "2026-06-19T14:30:00.0000000" || etz != "America/New_York" {
		t.Errorf("EndGraph = (%q,%q,%v), want (2026-06-19T14:30:00.0000000, America/New_York, true)", ed, etz, ok)
	}
}

// A floating Start (no TimeZone) reports "UTC" to Graph.
func TestStartGraphFloatingIsUTC(t *testing.T) {
	e := Event{}
	e.SetUTCTimes(time.Date(2026, 6, 19, 13, 0, 0, 0, time.UTC), time.Date(2026, 6, 19, 14, 0, 0, 0, time.UTC))
	_, stz, ok := e.StartGraph()
	if !ok || stz != "Etc/UTC" {
		t.Errorf("StartGraph zone = %q (ok=%v), want Etc/UTC", stz, ok)
	}
}

// A nominal day-valued Duration in a DST-observing zone, spanning the
// spring-forward transition, must keep the end at the same wall-clock time —
// iCalendar nominal-duration semantics. (Regression guard for the DST skew that
// applying the duration on the UTC instant introduced.)
func TestEndTimeDayDurationAcrossDSTStaysWallClock(t *testing.T) {
	ldt := jscalendar.LocalDateTime{Year: 2026, Month: 3, Day: 7, Hour: 12}
	e := Event{Event: jscalendar.Event{
		Start:    &ldt,
		TimeZone: "America/New_York",
		Duration: &jscalendar.Duration{Days: 2}, // P2D across the Mar 8 spring-forward
	}}
	ed, etz, ok := e.EndGraph()
	if !ok || ed != "2026-03-09T12:00:00.0000000" || etz != "America/New_York" {
		t.Errorf("EndGraph = (%q,%q,%v), want (2026-03-09T12:00:00.0000000, America/New_York, true) — wall-clock must be preserved across DST", ed, etz, ok)
	}
}

// An exact sub-day Duration is absolute time: one hour added across the
// spring-forward gap lands at 03:30 wall-clock (02:30 does not exist).
func TestEndTimeExactDurationAcrossDSTIsAbsolute(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	ldt := jscalendar.LocalDateTime{Year: 2026, Month: 3, Day: 8, Hour: 1, Minute: 30}
	e := Event{Event: jscalendar.Event{
		Start:    &ldt,
		TimeZone: "America/New_York",
		Duration: &jscalendar.Duration{Hours: 1},
	}}
	// 01:30 EST + 1h absolute = 03:30 EDT (the 02:30 wall time is skipped).
	wantUTC := time.Date(2026, 3, 8, 1, 30, 0, 0, loc).Add(time.Hour).UTC()
	if !e.EndTime().Equal(wantUTC) {
		t.Errorf("EndTime = %v, want %v", e.EndTime().UTC(), wantUTC)
	}
	ed, _, _ := e.EndGraph()
	if ed != "2026-03-08T03:30:00.0000000" {
		t.Errorf("EndGraph dateTime = %q, want 2026-03-08T03:30:00.0000000", ed)
	}
}

// Participant helpers: role filtering, deterministic ordering, email extraction.
func TestParticipantHelpers(t *testing.T) {
	org := NewParticipant("Alice", "alice@example.com", "", "owner")
	a1 := NewParticipant("Bob", "bob@example.com", "accepted", "attendee")
	a2 := NewParticipant("Carol", "carol@example.com", "needs-action", "attendee")
	e := Event{}
	e.SetOrganizerAttendees(&org, []jscalendar.Participant{a1, a2})

	gotOrg, ok := e.Organizer()
	if !ok || ParticipantEmail(gotOrg) != "alice@example.com" {
		t.Errorf("Organizer = %+v (ok=%v)", gotOrg, ok)
	}
	atts := e.Attendees()
	if len(atts) != 2 || ParticipantEmail(atts[0]) != "bob@example.com" || ParticipantEmail(atts[1]) != "carol@example.com" {
		t.Errorf("Attendees = %+v, want bob then carol", atts)
	}
	if got := PartStatToResponse(atts[1].ParticipationStatus); got != "notResponded" {
		t.Errorf("PartStatToResponse(needs-action) = %q, want notResponded", got)
	}
	if got := ResponseToPartStat("tentativelyAccepted"); got != "tentative" {
		t.Errorf("ResponseToPartStat(tentativelyAccepted) = %q, want tentative", got)
	}
}

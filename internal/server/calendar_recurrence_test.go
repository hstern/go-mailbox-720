package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

func TestGraphRecurrenceWeekly(t *testing.T) {
	start := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC) // a Monday
	pr, ok := graphRecurrence(&calendar.RecurrencePattern{RRULE: "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE"}, start)
	if !ok {
		t.Fatal("graphRecurrence returned ok=false for a weekly rule")
	}
	pat, _ := pr.Pattern.Get()
	if got, _ := pat.Type.Get(); got != api.MicrosoftGraphRecurrencePatternTypeWeekly {
		t.Errorf("pattern type = %v, want weekly", got)
	}
	if got, _ := pat.Interval.Get(); got != 2 {
		t.Errorf("interval = %d, want 2", got)
	}
	if got := pat.DaysOfWeek; len(got) != 2 {
		t.Errorf("daysOfWeek = %v, want 2", got)
	}
	rng, _ := pr.Range.Get()
	if got, _ := rng.Type.Get(); got != api.MicrosoftGraphRecurrenceRangeTypeNoEnd {
		t.Errorf("range type = %v, want noEnd", got)
	}
}

func TestRecurrenceFromGraphWeekly(t *testing.T) {
	var pat api.MicrosoftGraphRecurrencePattern
	pat.SetType(api.NewOptMicrosoftGraphRecurrencePatternType(api.MicrosoftGraphRecurrencePatternTypeWeekly))
	pat.SetInterval(api.NewOptInt32(2))
	pat.SetDaysOfWeek([]api.MicrosoftGraphDayOfWeek{api.MicrosoftGraphDayOfWeekMonday, api.MicrosoftGraphDayOfWeekWednesday})

	rp, err := recurrenceFromGraph(api.MicrosoftGraphPatternedRecurrence{
		Pattern: api.NewOptMicrosoftGraphRecurrencePattern(pat),
	})
	if err != nil {
		t.Fatalf("recurrenceFromGraph: %v", err)
	}
	if rp == nil {
		t.Fatal("recurrenceFromGraph returned nil for a weekly pattern")
	}
	for _, want := range []string{"FREQ=WEEKLY", "INTERVAL=2", "BYDAY=MO,WE"} {
		if !strings.Contains(rp.RRULE, want) {
			t.Errorf("RRULE %q missing %q", rp.RRULE, want)
		}
	}
}

// fakeInstanceBackend adds occurrence expansion (InstanceReader) to the fake.
type fakeInstanceBackend struct {
	*fakeCalendarBackend
	instances []calendar.Event
}

func (f *fakeInstanceBackend) ListInstances(_ context.Context, _ string, _ calendar.Range) ([]calendar.Event, error) {
	return f.instances, nil
}

type fakeInstanceProvider struct{ b calendar.Backend }

func (p fakeInstanceProvider) Calendar(_ context.Context) (calendar.Backend, error) { return p.b, nil }

func TestMeEventsListInstances(t *testing.T) {
	first := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 7)
	backend := &fakeInstanceBackend{
		fakeCalendarBackend: newCalendarFixture(),
		instances: []calendar.Event{
			{ID: "evt-1", Subject: "Standup", Start: first, End: first.Add(time.Hour), RecurrenceID: first, SeriesMasterID: "evt-1"},
			{ID: "evt-1", Subject: "Standup", Start: second, End: second.Add(time.Hour), RecurrenceID: second, SeriesMasterID: "evt-1"},
		},
	}
	h := Handler{calendar: fakeInstanceProvider{b: backend}}

	res, err := h.MeEventsListInstances(context.Background(), api.MeEventsListInstancesParams{
		EventID:       "evt-1",
		StartDateTime: "2026-06-15T00:00:00Z",
		EndDateTime:   "2026-06-30T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("MeEventsListInstances: %v", err)
	}
	ok, isOK := res.(*api.MicrosoftGraphEventCollectionResponseStatusCode)
	if !isOK {
		t.Fatalf("response type = %T, want collection", res)
	}
	if got := len(ok.Response.Value); got != 2 {
		t.Fatalf("instance count = %d, want 2", got)
	}
}

func TestMeEventsListInstancesRejectsBadWindow(t *testing.T) {
	backend := &fakeInstanceBackend{fakeCalendarBackend: newCalendarFixture()}
	h := Handler{calendar: fakeInstanceProvider{b: backend}}

	res, err := h.MeEventsListInstances(context.Background(), api.MeEventsListInstancesParams{
		EventID: "evt-1", StartDateTime: "not-a-time", EndDateTime: "2026-06-30T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, isErr := res.(*api.ErrorStatusCode); !isErr {
		t.Fatalf("response type = %T, want *ErrorStatusCode (400)", res)
	}
}

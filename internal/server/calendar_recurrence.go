package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	ht "github.com/ogen-go/ogen/http"
	"github.com/teambition/rrule-go"

	"github.com/hstern/go-jscalendar"

	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/graph/api"
)

// graphRecurrence maps the event's structured JSCalendar recurrence rules onto
// Graph's patternedRecurrence (recurrencePattern + recurrenceRange). It derives an
// RFC 5545 RRULE string from the structured rules via calendar.RRULEFromRules
// (anchored at the event's Start/TimeZone so BY* parts emit correctly) and feeds it
// through the same RRULE->Graph-pattern logic as before. It returns ok=false when
// the event has no rules, the RRULE cannot be derived/parsed, or it uses a
// frequency the Graph shape does not model (e.g. HOURLY), so the caller simply
// omits event.recurrence rather than emitting a malformed one.
//
// The mapping covers the common Exchange patterns: DAILY, WEEKLY (BYDAY ->
// daysOfWeek), and absolute MONTHLY/YEARLY (BYMONTHDAY -> dayOfMonth, BYMONTH ->
// month). INTERVAL maps to pattern.interval; COUNT -> numbered range, UNTIL ->
// endDate range, neither -> noEnd. Relative monthly/yearly (BYDAY with an ordinal,
// e.g. "the second Tuesday") is reported unmapped — a deliberate first-cut bound.
func graphRecurrence(e calendar.Event) (api.MicrosoftGraphPatternedRecurrence, bool) {
	if len(e.RecurrenceRules) == 0 {
		return api.MicrosoftGraphPatternedRecurrence{}, false
	}
	rruleStr, err := calendar.RRULEFromRules(e.Start, e.TimeZone, e.RecurrenceRules)
	if err != nil || strings.TrimSpace(rruleStr) == "" {
		return api.MicrosoftGraphPatternedRecurrence{}, false
	}
	seriesStart := e.StartTime()
	opt, err := rrule.StrToROption(rruleStr)
	if err != nil {
		return api.MicrosoftGraphPatternedRecurrence{}, false
	}

	var pattern api.MicrosoftGraphRecurrencePattern
	interval := opt.Interval
	if interval < 1 {
		interval = 1
	}
	pattern.SetInterval(api.NewOptInt32(int32(interval)))

	switch opt.Freq {
	case rrule.DAILY:
		pattern.SetType(api.NewOptMicrosoftGraphRecurrencePatternType(api.MicrosoftGraphRecurrencePatternTypeDaily))
	case rrule.WEEKLY:
		pattern.SetType(api.NewOptMicrosoftGraphRecurrencePatternType(api.MicrosoftGraphRecurrencePatternTypeWeekly))
		days, ok := graphDaysOfWeek(opt.Byweekday)
		if !ok {
			return api.MicrosoftGraphPatternedRecurrence{}, false
		}
		if len(days) == 0 && !seriesStart.IsZero() {
			// A bare WEEKLY rule recurs on the start's weekday.
			days = []api.MicrosoftGraphDayOfWeek{graphDayOfWeek(seriesStart.Weekday())}
		}
		pattern.SetDaysOfWeek(days)
	case rrule.MONTHLY:
		if len(opt.Byweekday) > 0 {
			return api.MicrosoftGraphPatternedRecurrence{}, false // relativeMonthly: unmapped
		}
		pattern.SetType(api.NewOptMicrosoftGraphRecurrencePatternType(api.MicrosoftGraphRecurrencePatternTypeAbsoluteMonthly))
		pattern.SetDayOfMonth(api.NewOptInt32(int32(monthDay(opt, seriesStart))))
	case rrule.YEARLY:
		if len(opt.Byweekday) > 0 {
			return api.MicrosoftGraphPatternedRecurrence{}, false // relativeYearly: unmapped
		}
		pattern.SetType(api.NewOptMicrosoftGraphRecurrencePatternType(api.MicrosoftGraphRecurrencePatternTypeAbsoluteYearly))
		pattern.SetDayOfMonth(api.NewOptInt32(int32(monthDay(opt, seriesStart))))
		pattern.SetMonth(api.NewOptInt32(int32(yearMonth(opt, seriesStart))))
	default:
		return api.MicrosoftGraphPatternedRecurrence{}, false
	}

	rng := graphRange(opt, seriesStart)
	return api.MicrosoftGraphPatternedRecurrence{
		Pattern: api.NewOptMicrosoftGraphRecurrencePattern(pattern),
		Range:   api.NewOptMicrosoftGraphRecurrenceRange(rng),
	}, true
}

// graphRange maps an RRULE's COUNT/UNTIL onto a Graph recurrenceRange: COUNT ->
// numbered, UNTIL -> endDate, neither -> noEnd. The startDate is the series start.
func graphRange(opt *rrule.ROption, seriesStart time.Time) api.MicrosoftGraphRecurrenceRange {
	var rng api.MicrosoftGraphRecurrenceRange
	if !seriesStart.IsZero() {
		rng.SetStartDate(api.NewOptNilDate(seriesStart.UTC()))
	}
	rng.SetRecurrenceTimeZone(api.NewOptNilString("UTC"))
	switch {
	case opt.Count > 0:
		rng.SetType(api.NewOptMicrosoftGraphRecurrenceRangeType(api.MicrosoftGraphRecurrenceRangeTypeNumbered))
		rng.SetNumberOfOccurrences(api.NewOptInt32(int32(opt.Count)))
	case !opt.Until.IsZero():
		rng.SetType(api.NewOptMicrosoftGraphRecurrenceRangeType(api.MicrosoftGraphRecurrenceRangeTypeEndDate))
		rng.SetEndDate(api.NewOptNilDate(opt.Until.UTC()))
	default:
		rng.SetType(api.NewOptMicrosoftGraphRecurrenceRangeType(api.MicrosoftGraphRecurrenceRangeTypeNoEnd))
	}
	return rng
}

// monthDay returns the BYMONTHDAY of a monthly/yearly rule, falling back to the
// series start's day-of-month when the rule leaves it implicit.
func monthDay(opt *rrule.ROption, seriesStart time.Time) int {
	if len(opt.Bymonthday) > 0 {
		return opt.Bymonthday[0]
	}
	if !seriesStart.IsZero() {
		return seriesStart.Day()
	}
	return 1
}

// yearMonth returns the BYMONTH of a yearly rule, falling back to the series
// start's month.
func yearMonth(opt *rrule.ROption, seriesStart time.Time) int {
	if len(opt.Bymonth) > 0 {
		return opt.Bymonth[0]
	}
	if !seriesStart.IsZero() {
		return int(seriesStart.Month())
	}
	return 1
}

// graphDaysOfWeek maps RRULE BYDAY weekdays onto Graph dayOfWeek tokens. An
// ordinal weekday (e.g. "2MO") is reported unmapped (ok=false) since it belongs to
// the relative-monthly shape this first cut does not emit.
func graphDaysOfWeek(wdays []rrule.Weekday) ([]api.MicrosoftGraphDayOfWeek, bool) {
	out := make([]api.MicrosoftGraphDayOfWeek, 0, len(wdays))
	for _, w := range wdays {
		if w.N() != 0 {
			return nil, false
		}
		out = append(out, graphWeekday(w.Day()))
	}
	return out, true
}

// graphWeekday maps an rrule weekday index (0=Monday .. 6=Sunday) to the Graph
// dayOfWeek enum.
func graphWeekday(day int) api.MicrosoftGraphDayOfWeek {
	switch day {
	case 0:
		return api.MicrosoftGraphDayOfWeekMonday
	case 1:
		return api.MicrosoftGraphDayOfWeekTuesday
	case 2:
		return api.MicrosoftGraphDayOfWeekWednesday
	case 3:
		return api.MicrosoftGraphDayOfWeekThursday
	case 4:
		return api.MicrosoftGraphDayOfWeekFriday
	case 5:
		return api.MicrosoftGraphDayOfWeekSaturday
	default:
		return api.MicrosoftGraphDayOfWeekSunday
	}
}

// graphDayOfWeek maps a Go time.Weekday to the Graph dayOfWeek enum.
func graphDayOfWeek(d time.Weekday) api.MicrosoftGraphDayOfWeek {
	return [...]api.MicrosoftGraphDayOfWeek{
		api.MicrosoftGraphDayOfWeekSunday,
		api.MicrosoftGraphDayOfWeekMonday,
		api.MicrosoftGraphDayOfWeekTuesday,
		api.MicrosoftGraphDayOfWeekWednesday,
		api.MicrosoftGraphDayOfWeekThursday,
		api.MicrosoftGraphDayOfWeekFriday,
		api.MicrosoftGraphDayOfWeekSaturday,
	}[d]
}

// recurrenceFromGraph maps a Graph patternedRecurrence onto structured JSCalendar
// recurrence rules, the inverse of graphRecurrence for create/patch. It builds the
// RFC 5545 RRULE string from the Graph pattern as before, then parses it into the
// neutral []jscalendar.RecurrenceRule via calendar.RulesFromRRULE so the event
// carries structured rules. It returns nil rules when the pattern is absent or
// carries no usable type. Relative monthly/yearly types are rejected with an error,
// matching graphRecurrence's first-cut bound, so the caller can surface a 400
// rather than silently dropping the rule.
func recurrenceFromGraph(pr api.MicrosoftGraphPatternedRecurrence) ([]jscalendar.RecurrenceRule, error) {
	pat, ok := pr.Pattern.Get()
	if !ok {
		return nil, nil
	}
	typ, ok := pat.Type.Get()
	if !ok {
		return nil, nil
	}

	parts := []string{}
	interval := int(pat.Interval.Or(1))
	if interval < 1 {
		interval = 1
	}
	switch typ {
	case api.MicrosoftGraphRecurrencePatternTypeDaily:
		parts = append(parts, "FREQ=DAILY")
	case api.MicrosoftGraphRecurrencePatternTypeWeekly:
		parts = append(parts, "FREQ=WEEKLY")
		if days := rruleByDay(pat.DaysOfWeek); days != "" {
			parts = append(parts, "BYDAY="+days)
		}
	case api.MicrosoftGraphRecurrencePatternTypeAbsoluteMonthly:
		parts = append(parts, "FREQ=MONTHLY")
		if d, ok := pat.DayOfMonth.Get(); ok {
			parts = append(parts, fmt.Sprintf("BYMONTHDAY=%d", d))
		}
	case api.MicrosoftGraphRecurrencePatternTypeAbsoluteYearly:
		parts = append(parts, "FREQ=YEARLY")
		if m, ok := pat.Month.Get(); ok {
			parts = append(parts, fmt.Sprintf("BYMONTH=%d", m))
		}
		if d, ok := pat.DayOfMonth.Get(); ok {
			parts = append(parts, fmt.Sprintf("BYMONTHDAY=%d", d))
		}
	default:
		return nil, fmt.Errorf("recurrence pattern type %q is not supported", typ)
	}
	if interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", interval))
	}

	if rng, ok := pr.Range.Get(); ok {
		switch rng.Type.Or("") {
		case api.MicrosoftGraphRecurrenceRangeTypeNumbered:
			if n, ok := rng.NumberOfOccurrences.Get(); ok && n > 0 {
				parts = append(parts, fmt.Sprintf("COUNT=%d", n))
			}
		case api.MicrosoftGraphRecurrenceRangeTypeEndDate:
			if end, ok := rng.EndDate.Get(); ok {
				parts = append(parts, "UNTIL="+end.UTC().Format("20060102T150405Z"))
			}
		}
	}

	rules, err := calendar.RulesFromRRULE(strings.Join(parts, ";"))
	if err != nil {
		return nil, fmt.Errorf("recurrence: %w", err)
	}
	return rules, nil
}

// rruleByDay maps Graph dayOfWeek tokens onto the comma-joined RRULE BYDAY value
// (MO,TU,...). Order is preserved.
func rruleByDay(days []api.MicrosoftGraphDayOfWeek) string {
	codes := make([]string, 0, len(days))
	for _, d := range days {
		switch d {
		case api.MicrosoftGraphDayOfWeekMonday:
			codes = append(codes, "MO")
		case api.MicrosoftGraphDayOfWeekTuesday:
			codes = append(codes, "TU")
		case api.MicrosoftGraphDayOfWeekWednesday:
			codes = append(codes, "WE")
		case api.MicrosoftGraphDayOfWeekThursday:
			codes = append(codes, "TH")
		case api.MicrosoftGraphDayOfWeekFriday:
			codes = append(codes, "FR")
		case api.MicrosoftGraphDayOfWeekSaturday:
			codes = append(codes, "SA")
		case api.MicrosoftGraphDayOfWeekSunday:
			codes = append(codes, "SU")
		}
	}
	return strings.Join(codes, ",")
}

// graphEventType reports the Graph event.type value for an event: an override
// instance is an "exception", a synthesized occurrence is an "occurrence", a
// series master is a "seriesMaster", and anything else is a "singleInstance".
func graphEventType(e calendar.Event) api.MicrosoftGraphEventType {
	switch {
	case e.RecurrenceID != nil && e.IsOverride:
		return api.MicrosoftGraphEventTypeException
	case e.RecurrenceID != nil:
		return api.MicrosoftGraphEventTypeOccurrence
	case len(e.RecurrenceRules) > 0:
		return api.MicrosoftGraphEventTypeSeriesMaster
	default:
		return api.MicrosoftGraphEventTypeSingleInstance
	}
}

// MeEventsListInstances implements GET /me/events/{event-id}/instances: the
// occurrences of a recurring series within the [startDateTime, endDateTime] window,
// expanded from the master's RRULE with any per-occurrence overrides applied.
func (h Handler) MeEventsListInstances(ctx context.Context, params api.MeEventsListInstancesParams) (api.MeEventsListInstancesRes, error) {
	value, badMsg, err := h.eventInstances(ctx, params.EventID, params.StartDateTime, params.EndDateTime)
	if err != nil {
		return nil, err
	}
	if badMsg != "" {
		return badRequest(badMsg), nil
	}
	return &api.MicrosoftGraphEventCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphEventCollectionResponse{Value: value},
	}, nil
}

// eventInstances expands the recurring series eventID over the [start, end] window
// (both RFC 3339) via the backend's InstanceReader capability, mapping each
// occurrence to a Graph event. A non-empty badMsg is a 400 (an unparseable or empty
// window); a backend without recurrence expansion returns ht.ErrNotImplemented.
func (h Handler) eventInstances(ctx context.Context, eventID, start, end string) (value []api.MicrosoftGraphEvent, badMsg string, err error) {
	r, perr := instancesRange(start, end)
	if perr != nil {
		return nil, perr.Error(), nil
	}
	b, err := h.calendarBackend(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = b.Close() }()

	ir, ok := b.(calendar.InstanceReader)
	if !ok {
		return nil, "", ht.ErrNotImplemented
	}
	insts, err := ir.ListInstances(ctx, eventID, r)
	if err != nil {
		return nil, "", fmt.Errorf("list instances: %w", err)
	}
	value = make([]api.MicrosoftGraphEvent, 0, len(insts))
	for _, e := range insts {
		value = append(value, toGraphEvent(e))
	}
	return value, "", nil
}

// instancesRange parses the required startDateTime/endDateTime window (RFC 3339)
// that the /instances and /calendarView operations take as query parameters. The
// window must be non-empty: an unbounded RRULE has infinitely many occurrences.
func instancesRange(start, end string) (calendar.Range, error) {
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return calendar.Range{}, fmt.Errorf("startDateTime: %w", err)
	}
	e, err := time.Parse(time.RFC3339, end)
	if err != nil {
		return calendar.Range{}, fmt.Errorf("endDateTime: %w", err)
	}
	if !e.After(s) {
		return calendar.Range{}, fmt.Errorf("endDateTime must be after startDateTime")
	}
	return calendar.Range{Start: s, End: e}, nil
}

package caldav

import (
	"strings"

	"github.com/emersion/go-ical"
)

// partStatToJSCal maps an iCalendar ATTENDEE PARTSTAT (RFC 5545) to the
// JSCalendar participationStatus vocabulary the neutral model carries
// (RFC 8984 §4.4.6). An unrecognized or absent PARTSTAT yields "" (unset).
//
// The go-jscalendar/ical bridge maps ORGANIZER/ATTENDEE into Participants but
// does not carry PARTSTAT, so the adapter reconciles it onto the participants
// after the bridge runs (and emits it on the write path).
var partStatToJSCal = map[string]string{
	"ACCEPTED":     "accepted",
	"DECLINED":     "declined",
	"TENTATIVE":    "tentative",
	"NEEDS-ACTION": "needs-action",
}

// jsCalToPartStat is the inverse, for writing. An unrecognized participationStatus
// yields "" (the attendee is written with no PARTSTAT parameter).
var jsCalToPartStat = map[string]string{
	"accepted":     "ACCEPTED",
	"declined":     "DECLINED",
	"tentative":    "TENTATIVE",
	"needs-action": "NEEDS-ACTION",
}

// attendeePartStat reads the JSCalendar participationStatus from an ATTENDEE
// property's PARTSTAT parameter.
func attendeePartStat(prop *ical.Prop) string {
	if prop == nil {
		return ""
	}
	return partStatToJSCal[strings.ToUpper(prop.Params.Get(ical.ParamParticipationStatus))]
}

// paramScheduleStatus is the RFC 6638 SCHEDULE-STATUS ATTENDEE parameter, carrying
// the delivery status of a scheduling message. go-ical has no constant for it (it
// is a CalDAV-scheduling extension, not core iCalendar), so it is named here.
const paramScheduleStatus = "SCHEDULE-STATUS"

// attendeeScheduleStatus reads the RFC 6638 SCHEDULE-STATUS parameter from an
// ATTENDEE property. Like PARTSTAT, the go-jscalendar/ical bridge does not carry
// it, so the adapter reconciles it onto the participant's ScheduleStatus (and
// re-emits it on write) to round-trip the client-side scheduling delivery outcome
// the server records (see internal/server recordSchedulingOutcome).
func attendeeScheduleStatus(prop *ical.Prop) string {
	if prop == nil {
		return ""
	}
	return prop.Params.Get(paramScheduleStatus)
}

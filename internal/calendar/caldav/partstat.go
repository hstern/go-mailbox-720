package caldav

import (
	"strings"

	"github.com/emersion/go-ical"
)

// partStatToStatus maps an iCalendar ATTENDEE PARTSTAT (RFC 5545) to the neutral
// calendar.Attendee.Status (Graph responseStatus shape). An unrecognized or
// absent PARTSTAT yields "" (unset).
var partStatToStatus = map[string]string{
	"ACCEPTED":     "accepted",
	"DECLINED":     "declined",
	"TENTATIVE":    "tentativelyAccepted",
	"NEEDS-ACTION": "notResponded",
}

// statusToPartStat is the inverse, for writing. An unrecognized status yields ""
// (the attendee is written with no PARTSTAT parameter).
var statusToPartStat = map[string]string{
	"accepted":            "ACCEPTED",
	"declined":            "DECLINED",
	"tentativelyAccepted": "TENTATIVE",
	"notResponded":        "NEEDS-ACTION",
}

// attendeeStatus reads the neutral status from an ATTENDEE property's PARTSTAT.
func attendeeStatus(prop *ical.Prop) string {
	if prop == nil {
		return ""
	}
	return partStatToStatus[strings.ToUpper(prop.Params.Get(ical.ParamParticipationStatus))]
}

// paramScheduleStatus is the RFC 6638 SCHEDULE-STATUS ATTENDEE parameter, carrying
// the delivery status of a scheduling message. go-ical has no constant for it (it
// is a CalDAV-scheduling extension, not core iCalendar), so it is named here.
const paramScheduleStatus = "SCHEDULE-STATUS"

package caldav

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
	"time"
)

// calendarID returns an opaque, stable id for a CalDAV calendar collection. The
// collection path (href) round-trips, so the server can address the calendar
// again without server-side state. Mirrors imap.folderID.
func calendarID(calendarPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(calendarPath))
}

func decodeCalendarID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("invalid calendar id: %w", err)
	}
	return string(b), nil
}

// eventID encodes the CalDAV object path (href) that locates an event resource
// into one opaque id. A single calendar object resource carries one logical
// event (its master VEVENT plus any recurrence overrides), so the path alone
// addresses it. Mirrors imap.messageID.
func eventID(objectPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(objectPath))
}

func decodeEventID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("invalid event id: %w", err)
	}
	return string(b), nil
}

// instanceSep separates an object path from a recurrence-id in an instance event
// ID. A space cannot appear in a CalDAV href (RFC 3986 reserves it), so it
// unambiguously delimits the two halves inside the base64-encoded id.
const instanceSep = " "

// instanceIDLayout formats a RECURRENCE-ID instant into an instance event ID. It
// is a compact UTC timestamp (no separators) so the encoded id stays short and
// round-trips exactly.
const instanceIDLayout = "20060102T150405Z"

// instanceEventID encodes the (object path, recurrence-id) pair that addresses a
// single occurrence of a recurring series into one opaque id, distinct from the
// series master's eventID. The recurrence-id is the occurrence's original start in
// UTC; GetEvent decodes it back to fetch the object and select the instance.
func instanceEventID(objectPath string, recurrenceID time.Time) string {
	raw := objectPath + instanceSep + recurrenceID.UTC().Format(instanceIDLayout)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeInstanceEventID decodes an opaque id into its object path and, when the id
// addresses a single occurrence, the recurrence-id instant (ok=true). A plain
// event id (no instance separator) yields the object path with ok=false, so
// callers can treat a master id and an instance id uniformly.
func decodeInstanceEventID(id string) (objectPath string, recurrenceID time.Time, ok bool, err error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("invalid event id: %w", err)
	}
	s := string(b)
	sep := strings.LastIndex(s, instanceSep)
	if sep < 0 {
		return s, time.Time{}, false, nil
	}
	rid, err := time.Parse(instanceIDLayout, s[sep+len(instanceSep):])
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("invalid instance recurrence-id: %w", err)
	}
	return s[:sep], rid.UTC(), true, nil
}

// calendarIDForObject derives the opaque id of the calendar collection that
// contains an object path. CalDAV object resources live directly under their
// collection, so the parent directory is the collection path.
func calendarIDForObject(objectPath string) string {
	return calendarID(path.Dir(objectPath) + "/")
}

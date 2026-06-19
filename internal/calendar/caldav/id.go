package caldav

import (
	"encoding/base64"
	"fmt"
	"path"
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

// calendarIDForObject derives the opaque id of the calendar collection that
// contains an object path. CalDAV object resources live directly under their
// collection, so the parent directory is the collection path.
func calendarIDForObject(objectPath string) string {
	return calendarID(path.Dir(objectPath) + "/")
}

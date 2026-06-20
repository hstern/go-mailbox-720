// Package tz resolves a Microsoft Graph dateTimeTimeZone "timeZone" value into a
// Go *time.Location.
//
// Graph events carry their start and end as a {dateTime, timeZone} pair, and the
// timeZone string is one of three flavours:
//
//   - a WINDOWS zone display name — the common case — e.g. "Pacific Standard
//     Time" or "GMT Standard Time". These are CLDR Windows zone IDs, not IANA
//     names, and time.LoadLocation does not understand them.
//   - an IANA zone name, e.g. "America/Los_Angeles" or "Etc/UTC", which Graph
//     emits when a mailbox is configured to use IANA time zones.
//   - "UTC" (or the empty string, treated as UTC).
//
// [Lookup] handles all three. It maps the Windows display name through a curated
// subset of the CLDR windowsZones table to a primary IANA zone, then defers to
// time.LoadLocation; failing that it tries time.LoadLocation on the raw value
// directly so IANA names pass through unchanged.
//
// Resolution therefore depends on the system tzdata being available to
// time.LoadLocation. On a host or container without a zoneinfo database the
// load can fail with "unknown time zone"; a binary that needs to be
// self-contained can blank-import "time/tzdata" to embed the database. This
// package deliberately does NOT import time/tzdata — that is a binary-level
// decision left to the program that links us in.
package tz

import (
	"fmt"
	"strings"
	"time"
)

// Lookup resolves a Microsoft Graph timeZone string to a *time.Location.
//
// Resolution order:
//
//  1. "" or "UTC" (case-insensitively) -> time.UTC.
//  2. An exact match in the curated Windows->IANA table -> time.LoadLocation
//     on the mapped IANA name.
//  3. Otherwise time.LoadLocation on the raw name, which handles IANA names
//     ("America/New_York", "Etc/UTC", ...).
//
// If none of these resolve, Lookup returns an error wrapping [ErrUnknownZone]
// so callers can detect an unknown zone with errors.Is.
func Lookup(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "UTC") {
		return time.UTC, nil
	}

	if iana, ok := windowsToIANA[name]; ok {
		loc, err := time.LoadLocation(iana)
		if err != nil {
			return nil, fmt.Errorf("tz: loading IANA zone %q for Windows zone %q: %w", iana, name, err)
		}
		return loc, nil
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("tz: %q: %w: %v", name, ErrUnknownZone, err)
	}
	return loc, nil
}

// ErrUnknownZone is wrapped by the error [Lookup] returns when a timeZone string
// matches neither the curated Windows table nor any zone time.LoadLocation
// recognises. Detect it with errors.Is.
var ErrUnknownZone = fmt.Errorf("unknown time zone")

// windowsToIANA maps Windows zone display names (CLDR windowsZones IDs) to their
// primary IANA zone. This is the COMMON SUBSET of the full CLDR table — the
// widely-used zones across North America, Europe, Asia and Australia — not an
// exhaustive list. A Windows name absent from this table falls through to a
// direct time.LoadLocation attempt in [Lookup] and then to ErrUnknownZone.
//
// Each entry uses the "001" (primary territory) IANA mapping from CLDR's
// windowsZones.xml.
var windowsToIANA = map[string]string{
	// UTC and GMT
	"UTC":                     "Etc/UTC",
	"GMT Standard Time":       "Europe/London",
	"Greenwich Standard Time": "Atlantic/Reykjavik",

	// North America
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Aleutian Standard Time":          "America/Adak",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time":           "America/Los_Angeles",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"US Mountain Standard Time":       "America/Phoenix", // Arizona, no DST
	"Mountain Standard Time":          "America/Denver",
	"Mountain Standard Time (Mexico)": "America/Mazatlan",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"Eastern Standard Time":           "America/New_York",
	"Eastern Standard Time (Mexico)":  "America/Cancun",
	"US Eastern Standard Time":        "America/Indiana/Indianapolis",
	"Atlantic Standard Time":          "America/Halifax",
	"Newfoundland Standard Time":      "America/St_Johns",

	// Central & South America
	"Central America Standard Time":  "America/Guatemala",
	"SA Pacific Standard Time":       "America/Bogota",
	"SA Western Standard Time":       "America/La_Paz",
	"SA Eastern Standard Time":       "America/Cayenne",
	"Argentina Standard Time":        "America/Buenos_Aires",
	"E. South America Standard Time": "America/Sao_Paulo",
	"Pacific SA Standard Time":       "America/Santiago",
	"Venezuela Standard Time":        "America/Caracas",

	// Europe
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Central European Standard Time":  "Europe/Warsaw",
	"Romance Standard Time":           "Europe/Paris",
	"GTB Standard Time":               "Europe/Bucharest",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"FLE Standard Time":               "Europe/Kiev",
	"Russian Standard Time":           "Europe/Moscow",
	"Kaliningrad Standard Time":       "Europe/Kaliningrad",
	"Turkey Standard Time":            "Europe/Istanbul",
	"W. Central Africa Standard Time": "Africa/Lagos",

	// Africa & Middle East
	"South Africa Standard Time": "Africa/Johannesburg",
	"Egypt Standard Time":        "Africa/Cairo",
	"E. Africa Standard Time":    "Africa/Nairobi",
	"Israel Standard Time":       "Asia/Jerusalem",
	"Arabic Standard Time":       "Asia/Baghdad",
	"Arab Standard Time":         "Asia/Riyadh",
	"Arabian Standard Time":      "Asia/Dubai",
	"Iran Standard Time":         "Asia/Tehran",

	// South & Central Asia
	"Pakistan Standard Time":     "Asia/Karachi",
	"India Standard Time":        "Asia/Kolkata",
	"Sri Lanka Standard Time":    "Asia/Colombo",
	"Bangladesh Standard Time":   "Asia/Dhaka",
	"Nepal Standard Time":        "Asia/Kathmandu",
	"Afghanistan Standard Time":  "Asia/Kabul",
	"West Asia Standard Time":    "Asia/Tashkent",
	"Central Asia Standard Time": "Asia/Almaty",

	// East & Southeast Asia
	"China Standard Time":           "Asia/Shanghai",
	"Taipei Standard Time":          "Asia/Taipei",
	"Tokyo Standard Time":           "Asia/Tokyo",
	"Korea Standard Time":           "Asia/Seoul",
	"Singapore Standard Time":       "Asia/Singapore",
	"SE Asia Standard Time":         "Asia/Bangkok",
	"Myanmar Standard Time":         "Asia/Yangon",
	"W. Mongolia Standard Time":     "Asia/Hovd",
	"North Asia Standard Time":      "Asia/Krasnoyarsk",
	"North Asia East Standard Time": "Asia/Irkutsk",
	"Ulaanbaatar Standard Time":     "Asia/Ulaanbaatar",

	// Australia & Pacific
	"W. Australia Standard Time":    "Australia/Perth",
	"AUS Central Standard Time":     "Australia/Darwin",
	"Cen. Australia Standard Time":  "Australia/Adelaide",
	"AUS Eastern Standard Time":     "Australia/Sydney",
	"E. Australia Standard Time":    "Australia/Brisbane",
	"Tasmania Standard Time":        "Australia/Hobart",
	"West Pacific Standard Time":    "Pacific/Port_Moresby",
	"Central Pacific Standard Time": "Pacific/Guadalcanal",
	"New Zealand Standard Time":     "Pacific/Auckland",
	"Fiji Standard Time":            "Pacific/Fiji",
	"Tonga Standard Time":           "Pacific/Tongatapu",
	"Samoa Standard Time":           "Pacific/Apia",
}

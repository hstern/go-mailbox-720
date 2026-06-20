package tz

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// skipIfNoTzdata skips a subtest when the host has no zoneinfo database, so a
// missing tzdata on the runner does not fail the IANA-dependent assertions. The
// curated table lookups themselves are still exercised before this point.
func skipIfNoTzdata(t *testing.T, err error) {
	t.Helper()
	// time.LoadLocation reports a missing database as "unknown time zone <name>".
	if err != nil && strings.Contains(err.Error(), "unknown time zone") {
		t.Skipf("tzdata unavailable on this runner: %v", err)
	}
}

func TestLookupUTC(t *testing.T) {
	for _, name := range []string{"", "UTC", "utc", "Utc"} {
		loc, err := Lookup(name)
		if err != nil {
			t.Fatalf("Lookup(%q) returned error: %v", name, err)
		}
		if loc != time.UTC {
			t.Errorf("Lookup(%q) = %v, want time.UTC", name, loc)
		}
	}
}

func TestLookupBogusZone(t *testing.T) {
	_, err := Lookup("Totally Bogus Zone")
	if err == nil {
		t.Fatal("Lookup(\"Totally Bogus Zone\") = nil error, want error")
	}
	if !errors.Is(err, ErrUnknownZone) {
		t.Errorf("Lookup error = %v, want wrapping ErrUnknownZone", err)
	}
}

func TestWindowsTableMappings(t *testing.T) {
	// These assertions exercise the curated table directly and must hold
	// regardless of whether tzdata is available on the runner.
	want := map[string]string{
		"Pacific Standard Time":          "America/Los_Angeles",
		"Mountain Standard Time":         "America/Denver",
		"Central Standard Time":          "America/Chicago",
		"Eastern Standard Time":          "America/New_York",
		"US Mountain Standard Time":      "America/Phoenix",
		"Hawaiian Standard Time":         "Pacific/Honolulu",
		"Alaskan Standard Time":          "America/Anchorage",
		"UTC":                            "Etc/UTC",
		"GMT Standard Time":              "Europe/London",
		"W. Europe Standard Time":        "Europe/Berlin",
		"Central Europe Standard Time":   "Europe/Budapest",
		"Central European Standard Time": "Europe/Warsaw",
		"Romance Standard Time":          "Europe/Paris",
		"GTB Standard Time":              "Europe/Bucharest",
		"E. Europe Standard Time":        "Europe/Chisinau",
		"India Standard Time":            "Asia/Kolkata",
		"China Standard Time":            "Asia/Shanghai",
		"Tokyo Standard Time":            "Asia/Tokyo",
		"AUS Eastern Standard Time":      "Australia/Sydney",
	}
	for win, iana := range want {
		got, ok := windowsToIANA[win]
		if !ok {
			t.Errorf("windowsToIANA missing %q", win)
			continue
		}
		if got != iana {
			t.Errorf("windowsToIANA[%q] = %q, want %q", win, got, iana)
		}
	}
}

func TestLookupPacificStandardTime(t *testing.T) {
	loc, err := Lookup("Pacific Standard Time")
	skipIfNoTzdata(t, err)
	if err != nil {
		t.Fatalf("Lookup(\"Pacific Standard Time\") error: %v", err)
	}
	// Pick a fixed standard-time (winter) date to avoid DST brittleness.
	// On 2021-01-15 Pacific is on PST = UTC-8.
	at := time.Date(2021, time.January, 15, 12, 0, 0, 0, loc)
	_, offset := at.Zone()
	const wantOffset = -8 * 60 * 60
	if offset != wantOffset {
		t.Errorf("Pacific offset on 2021-01-15 = %d s, want %d s (UTC-8)", offset, wantOffset)
	}
}

func TestLookupIANAPassthrough(t *testing.T) {
	loc, err := Lookup("America/New_York")
	skipIfNoTzdata(t, err)
	if err != nil {
		t.Fatalf("Lookup(\"America/New_York\") error: %v", err)
	}
	if loc.String() != "America/New_York" {
		t.Errorf("loc.String() = %q, want \"America/New_York\"", loc.String())
	}
	// 2021-01-15 Eastern is on EST = UTC-5.
	at := time.Date(2021, time.January, 15, 12, 0, 0, 0, loc)
	_, offset := at.Zone()
	const wantOffset = -5 * 60 * 60
	if offset != wantOffset {
		t.Errorf("Eastern offset on 2021-01-15 = %d s, want %d s (UTC-5)", offset, wantOffset)
	}
}

func TestLookupTokyo(t *testing.T) {
	loc, err := Lookup("Tokyo Standard Time")
	skipIfNoTzdata(t, err)
	if err != nil {
		t.Fatalf("Lookup(\"Tokyo Standard Time\") error: %v", err)
	}
	if loc.String() != "Asia/Tokyo" {
		t.Errorf("loc.String() = %q, want \"Asia/Tokyo\"", loc.String())
	}
	// Japan has no DST; offset is UTC+9 year-round. Check a summer date too.
	at := time.Date(2021, time.July, 15, 12, 0, 0, 0, loc)
	_, offset := at.Zone()
	const wantOffset = 9 * 60 * 60
	if offset != wantOffset {
		t.Errorf("Tokyo offset = %d s, want %d s (UTC+9)", offset, wantOffset)
	}
}

package caldav

import "testing"

func TestCalendarIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"simple", "/calendars/alice/work/"},
		{"root", "/"},
		{"with spaces", "/calendars/alice/My Calendar/"},
		{"non-ascii", "/calendars/alice/Café/"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := calendarID(tt.path)
			got, err := decodeCalendarID(id)
			if err != nil {
				t.Fatalf("decodeCalendarID(%q) error: %v", id, err)
			}
			if got != tt.path {
				t.Errorf("round trip = %q, want %q", got, tt.path)
			}
		})
	}
}

func TestEventIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"simple", "/calendars/alice/work/event-123.ics"},
		{"uuid", "/calendars/alice/work/0fc3d1a4-8c2b-4f1e-9d77-abc.ics"},
		{"with spaces", "/calendars/alice/work/My Event.ics"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := eventID(tt.path)
			got, err := decodeEventID(id)
			if err != nil {
				t.Fatalf("decodeEventID(%q) error: %v", id, err)
			}
			if got != tt.path {
				t.Errorf("round trip = %q, want %q", got, tt.path)
			}
		})
	}
}

func TestDecodeInvalidIDs(t *testing.T) {
	// "!!!" is not valid base64url.
	if _, err := decodeCalendarID("!!!"); err == nil {
		t.Error("decodeCalendarID(invalid) = nil error, want error")
	}
	if _, err := decodeEventID("!!!"); err == nil {
		t.Error("decodeEventID(invalid) = nil error, want error")
	}
}

func TestCalendarIDForObject(t *testing.T) {
	objectPath := "/calendars/alice/work/event-123.ics"
	got := calendarIDForObject(objectPath)
	want := calendarID("/calendars/alice/work/")
	if got != want {
		t.Errorf("calendarIDForObject(%q) = %q, want %q", objectPath, got, want)
	}
}

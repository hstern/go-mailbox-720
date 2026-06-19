package carddav

import "testing"

func TestAddressBookIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"simple", "/addressbooks/alice/contacts/"},
		{"root", "/"},
		{"with spaces", "/addressbooks/alice/My Contacts/"},
		{"non-ascii", "/addressbooks/alice/Café/"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := addressBookID(tt.path)
			got, err := decodeAddressBookID(id)
			if err != nil {
				t.Fatalf("decodeAddressBookID(%q) error: %v", id, err)
			}
			if got != tt.path {
				t.Errorf("round trip = %q, want %q", got, tt.path)
			}
		})
	}
}

func TestContactIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"simple", "/addressbooks/alice/contacts/alice.vcf"},
		{"uuid", "/addressbooks/alice/contacts/4fbe8971-0bc3-424c.vcf"},
		{"with spaces", "/addressbooks/alice/contacts/My Card.vcf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := contactID(tt.path)
			got, err := decodeContactID(id)
			if err != nil {
				t.Fatalf("decodeContactID(%q) error: %v", id, err)
			}
			if got != tt.path {
				t.Errorf("round trip = %q, want %q", got, tt.path)
			}
		})
	}
}

func TestDecodeInvalidIDs(t *testing.T) {
	// "!!!" is not valid base64url.
	if _, err := decodeAddressBookID("!!!"); err == nil {
		t.Error("decodeAddressBookID(invalid) = nil error, want error")
	}
	if _, err := decodeContactID("!!!"); err == nil {
		t.Error("decodeContactID(invalid) = nil error, want error")
	}
}

func TestAddressBookIDForObject(t *testing.T) {
	objectPath := "/addressbooks/alice/contacts/alice.vcf"
	got := addressBookIDForObject(objectPath)
	want := addressBookID("/addressbooks/alice/contacts/")
	if got != want {
		t.Errorf("addressBookIDForObject(%q) = %q, want %q", objectPath, got, want)
	}
}

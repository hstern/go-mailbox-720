package carddav

import (
	"encoding/base64"
	"fmt"
	"path"
)

// addressBookID returns an opaque, stable id for a CardDAV address-book
// collection. The collection path (href) round-trips, so the server can address
// the address book again without server-side state. Mirrors caldav.calendarID.
func addressBookID(addressBookPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(addressBookPath))
}

func decodeAddressBookID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("invalid address book id: %w", err)
	}
	return string(b), nil
}

// contactID encodes the CardDAV object path (href) that locates an address
// object resource into one opaque id. A single address object resource carries
// one vCard, so the path alone addresses it. Mirrors caldav.eventID.
func contactID(objectPath string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(objectPath))
}

func decodeContactID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("invalid contact id: %w", err)
	}
	return string(b), nil
}

// addressBookIDForObject derives the opaque id of the address-book collection
// that contains an object path. CardDAV object resources live directly under
// their collection, so the parent directory is the collection path.
func addressBookIDForObject(objectPath string) string {
	return addressBookID(path.Dir(objectPath) + "/")
}

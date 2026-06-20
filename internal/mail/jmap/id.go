package jmap

import (
	"encoding/base64"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// JMAP object ids are already opaque, stable, server-assigned strings, so the
// port's opaque folder/message ids carry them near-verbatim. They are base64url
// wrapped only so the ids are URL-safe in a Graph path segment and consistent
// with the IMAP adapter's opaque-id posture — there is no extra structure to
// encode (unlike IMAP, where the tuple mailbox/UIDVALIDITY/UID must be packed).

// folderID returns an opaque, stable port id for a JMAP mailbox id.
func folderID(id gojmap.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeFolderID(id string) (gojmap.ID, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("jmap: invalid folder id: %w", err)
	}
	return gojmap.ID(b), nil
}

// messageID returns an opaque, stable port id for a JMAP email id.
func messageID(id gojmap.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeMessageID(id string) (gojmap.ID, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("jmap: invalid message id: %w", err)
	}
	return gojmap.ID(b), nil
}

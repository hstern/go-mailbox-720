package imap

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// folderID returns an opaque, stable id for an IMAP mailbox name. The name round
// trips, so the server can address the folder again without server-side state.
func folderID(mailbox string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(mailbox))
}

func decodeFolderID(id string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", fmt.Errorf("invalid folder id: %w", err)
	}
	return string(b), nil
}

// messageID encodes the tuple that locates a message — mailbox, UIDVALIDITY, and
// UID — into one opaque id. UIDVALIDITY is carried so a stale id (the folder was
// recreated) can be detected rather than silently returning the wrong message.
func messageID(mailbox string, uidValidity, uid uint32) string {
	raw := mailbox + "\x00" + strconv.FormatUint(uint64(uidValidity), 10) + "\x00" + strconv.FormatUint(uint64(uid), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeMessageID(id string) (mailbox string, uidValidity, uid uint32, err error) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid message id: %w", err)
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) != 3 {
		return "", 0, 0, fmt.Errorf("invalid message id structure")
	}
	uv, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid message id uidvalidity: %w", err)
	}
	u, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid message id uid: %w", err)
	}
	return parts[0], uint32(uv), uint32(u), nil
}

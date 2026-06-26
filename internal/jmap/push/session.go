package push

import (
	"encoding/json"

	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// CapabilityURI is the JMAP capability URN that advertises WebSocket support
// (RFC 8887 §4). Its value object carries the WebSocket endpoint URL.
const CapabilityURI gojmap.URI = "urn:ietf:params:jmap:websocket"

// WebSocketCapability is the value of the urn:ietf:params:jmap:websocket
// capability in a JMAP Session: the URL to open the socket to, and whether the
// server supports push (WebSocketPushEnable / StateChange) over it. A server may
// advertise the capability for request/response only, with SupportsPush false.
type WebSocketCapability struct {
	URL          string `json:"url"`
	SupportsPush bool   `json:"supportsPush"`
}

// WebSocketURL reads the RFC 8887 WebSocket capability from a JMAP session.
// go-jmap does not register this capability, so it is decoded from the session's
// raw capability map rather than the typed Capabilities. ok is false when the
// server advertises no WebSocket support or the capability is malformed.
func WebSocketURL(s *gojmap.Session) (capability WebSocketCapability, ok bool) {
	if s == nil {
		return WebSocketCapability{}, false
	}
	raw, present := s.RawCapabilities[CapabilityURI]
	if !present {
		return WebSocketCapability{}, false
	}
	if err := json.Unmarshal(raw, &capability); err != nil || capability.URL == "" {
		return WebSocketCapability{}, false
	}
	return capability, true
}

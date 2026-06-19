// Package subscriptions implements the core of Microsoft Graph change-notification
// subscriptions (https://learn.microsoft.com/graph/webhooks).
//
// A Graph client asks to be told when a resource changes by POSTing a
// subscription:
//
//	{"changeType":"created,updated","notificationUrl":"https://app.example/notify",
//	 "resource":"/me/messages","expirationDateTime":"2026-06-22T00:00:00Z",
//	 "clientState":"opaque-secret","lifecycleNotificationUrl":"..."}
//
// Before accepting the subscription, Graph proves the client owns the
// notificationUrl with a validation handshake: it POSTs that URL a
// "validationToken" query parameter, and the endpoint must echo the token back
// as text/plain with a 200 within 10 seconds. Subscriptions then expire — the
// maximum lifetime depends on the resource (messages are ~3 days) — and a client
// renews one by PATCHing its expirationDateTime. When the watched resource
// changes, Graph POSTs the notificationUrl a payload:
//
//	{"value":[{"subscriptionId":"...","clientState":"opaque-secret",
//	  "changeType":"updated","resource":"/me/messages/AAMk...",
//	  "resourceData":{"@odata.type":"#Microsoft.Graph.Message",
//	    "@odata.id":"/me/messages/AAMk...","id":"AAMk..."}}]}
//
// and the subscriber compares the echoed clientState to its stored secret to
// detect tampering.
//
// This package is the FIRST CUT: the model, store, validation, and the webhook
// validation handshake only. It defines the [Subscription] type and its
// [ChangeType], [Validate]s an incoming subscription request, persists
// subscriptions through a [Store] (with a concurrency-safe in-memory
// implementation), performs the notificationUrl handshake
// ([VerifyNotificationURL]), and builds the change-notification envelope a
// delivery loop will later POST ([NotificationPayload]). Wiring this into the
// HTTP handlers (POST /subscriptions) and driving delivery from IMAP IDLE are
// deferred to follow-up issues.
package subscriptions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ChangeType is the kind of change a subscription wants to be notified about,
// matching the Graph "changeType" values. A subscription may combine several as
// a comma-separated list (e.g. "created,updated").
type ChangeType string

const (
	// ChangeCreated fires when a matching resource is created.
	ChangeCreated ChangeType = "created"
	// ChangeUpdated fires when a matching resource is updated.
	ChangeUpdated ChangeType = "updated"
	// ChangeDeleted fires when a matching resource is deleted.
	ChangeDeleted ChangeType = "deleted"
)

// validChangeType reports whether a single token names a known change type.
func validChangeType(ct ChangeType) bool {
	switch ct {
	case ChangeCreated, ChangeUpdated, ChangeDeleted:
		return true
	default:
		return false
	}
}

// Subscription is a Graph change-notification subscription: which resource and
// change kinds to watch, where to deliver notifications, the opaque clientState
// echoed back for tamper detection, and the expiry/creation bookkeeping.
type Subscription struct {
	ID                 string
	Resource           string
	ChangeType         ChangeType
	NotificationURL    string
	ClientState        string
	ExpirationDateTime time.Time
	CreatedAt          time.Time
}

// Validation sentinel errors. Each names exactly one rejection reason so callers
// (a future POST /subscriptions handler) can map them to a Graph 400 with a
// specific message and tests can assert on them with errors.Is.
var (
	// ErrNotificationURLRequired is returned when notificationUrl is empty.
	ErrNotificationURLRequired = errors.New("subscriptions: notificationUrl is required")
	// ErrNotificationURLNotHTTPS is returned when notificationUrl is not https.
	ErrNotificationURLNotHTTPS = errors.New("subscriptions: notificationUrl must be https")
	// ErrInvalidChangeType is returned when changeType is empty or names an
	// unknown change kind.
	ErrInvalidChangeType = errors.New("subscriptions: changeType is invalid")
	// ErrUnsupportedResource is returned when resource is not an allowed resource.
	ErrUnsupportedResource = errors.New("subscriptions: resource is not supported")
	// ErrExpirationInPast is returned when expirationDateTime is at or before now.
	ErrExpirationInPast = errors.New("subscriptions: expirationDateTime is in the past")
	// ErrExpirationTooFar is returned when expirationDateTime exceeds now+maxTTL.
	ErrExpirationTooFar = errors.New("subscriptions: expirationDateTime exceeds the maximum lifetime")
)

// Validate checks an incoming subscription request against the Graph rules,
// returning one of the sentinel errors above (wrapped with context) on the first
// violation it finds:
//
//   - notificationUrl must be present and an https URL;
//   - changeType must be a comma-separated list of known change kinds, each
//     non-empty;
//   - resource must be one of allowedResources (case-insensitive);
//   - expirationDateTime must be after now and no later than now+maxTTL.
//
// now is the reference time (injected for testability) and maxTTL is the largest
// lifetime permitted for the resource. Validate does not mutate req.
func Validate(req Subscription, now time.Time, maxTTL time.Duration, allowedResources []string) error {
	if req.NotificationURL == "" {
		return ErrNotificationURLRequired
	}
	u, err := url.Parse(req.NotificationURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotificationURLNotHTTPS, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: scheme %q", ErrNotificationURLNotHTTPS, u.Scheme)
	}

	if err := validateChangeType(req.ChangeType); err != nil {
		return err
	}

	if !resourceAllowed(req.Resource, allowedResources) {
		return fmt.Errorf("%w: %q", ErrUnsupportedResource, req.Resource)
	}

	if !req.ExpirationDateTime.After(now) {
		return fmt.Errorf("%w: %s", ErrExpirationInPast, req.ExpirationDateTime.Format(time.RFC3339))
	}
	if req.ExpirationDateTime.After(now.Add(maxTTL)) {
		return fmt.Errorf("%w: %s is beyond %s", ErrExpirationTooFar,
			req.ExpirationDateTime.Format(time.RFC3339), now.Add(maxTTL).Format(time.RFC3339))
	}
	return nil
}

// validateChangeType checks that changeType is a non-empty, comma-separated list
// of known change kinds.
func validateChangeType(ct ChangeType) error {
	if ct == "" {
		return fmt.Errorf("%w: empty", ErrInvalidChangeType)
	}
	for _, part := range strings.Split(string(ct), ",") {
		token := ChangeType(strings.TrimSpace(part))
		if !validChangeType(token) {
			return fmt.Errorf("%w: %q", ErrInvalidChangeType, part)
		}
	}
	return nil
}

// resourceAllowed reports whether resource matches one of allowed,
// case-insensitively. An empty allow-list permits nothing.
func resourceAllowed(resource string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(resource, a) {
			return true
		}
	}
	return false
}

// VerifyNotificationURL performs the Graph notificationUrl validation handshake
// against notificationURL: it generates a random validationToken, POSTs the URL
// with the token as a "validationToken" query parameter, and requires a 200
// whose (trimmed) body equals the token. The whole exchange is bounded by a
// 10-second timeout derived from ctx. client must be non-nil.
//
// This mirrors what Graph does before accepting a subscription; a future POST
// /subscriptions handler calls it to prove the client owns the endpoint.
//
// SECURITY: notificationURL is client-supplied, so this is a server-side request
// forgery (SSRF) primitive. It MUST NOT be reachable from a live POST
// /subscriptions handler until host filtering is added — reject private,
// loopback, link-local, ULA and cloud-metadata (169.254.169.254) destinations,
// enforced at dial time (an http.Transport DialContext/Control hook) so DNS
// rebinding is caught as well. As a baseline this function refuses redirects, so
// an https URL cannot 302 to an internal http target and bypass the scheme gate;
// it does NOT yet IP-filter (tracked on MB720-9). client must be non-nil.
func VerifyNotificationURL(ctx context.Context, client *http.Client, notificationURL string) error {
	if client == nil {
		return errors.New("subscriptions: nil http client")
	}
	// Refuse redirects on a shallow copy so the caller's client is untouched: a
	// followed redirect could send the POST to a host that passed the https/scheme
	// check (and any future host filter applied only to the original URL).
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client = &noRedirect
	token, err := randomToken()
	if err != nil {
		return fmt.Errorf("subscriptions: generate validation token: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, validationTimeout)
	defer cancel()

	u, err := url.Parse(notificationURL)
	if err != nil {
		return fmt.Errorf("subscriptions: parse notificationUrl: %w", err)
	}
	q := u.Query()
	q.Set("validationToken", token)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("subscriptions: build validation request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("subscriptions: validation request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscriptions: validation returned status %d", resp.StatusCode)
	}
	// Read a bounded amount: the echoed token is short, so cap the body to guard
	// against a misbehaving endpoint streaming an unbounded response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxValidationBody))
	if err != nil {
		return fmt.Errorf("subscriptions: read validation body: %w", err)
	}
	if strings.TrimSpace(string(body)) != token {
		return errors.New("subscriptions: validation body did not echo the token")
	}
	return nil
}

const (
	// validationTimeout bounds the notificationUrl handshake. Graph allows the
	// endpoint 10 seconds to respond.
	validationTimeout = 10 * time.Second
	// maxValidationBody caps how much of the echoed body is read; the token is a
	// short hex string, so a few KiB is generous.
	maxValidationBody = 8 << 10 // 8 KiB
)

// resourceData is the per-change resource pointer in a notification: its OData
// type/id and the resource id.
type resourceData struct {
	ODataType string `json:"@odata.type"`
	ODataID   string `json:"@odata.id"`
	ID        string `json:"id"`
}

// changeNotification is one entry in a notification payload's "value" array.
type changeNotification struct {
	SubscriptionID string       `json:"subscriptionId"`
	ClientState    string       `json:"clientState,omitempty"`
	ChangeType     ChangeType   `json:"changeType"`
	Resource       string       `json:"resource"`
	ResourceData   resourceData `json:"resourceData"`
}

// notificationEnvelope wraps the change notifications Graph POSTs to a
// notificationUrl.
type notificationEnvelope struct {
	Value []changeNotification `json:"value"`
}

// NotificationPayload builds the JSON change-notification envelope for a single
// change to resourceID under sub, matching the shape Graph POSTs to a
// notificationUrl. The resource is sub.Resource with the changed item's id
// appended (e.g. "/me/messages/AAMk..."), and clientState is echoed so the
// subscriber can verify it. A delivery loop POSTs the returned bytes later.
func NotificationPayload(sub Subscription, resourceID string) ([]byte, error) {
	resource := strings.TrimRight(sub.Resource, "/") + "/" + resourceID
	env := notificationEnvelope{
		Value: []changeNotification{{
			SubscriptionID: sub.ID,
			ClientState:    sub.ClientState,
			ChangeType:     sub.ChangeType,
			Resource:       resource,
			ResourceData: resourceData{
				ODataType: "#Microsoft.Graph.Message",
				ODataID:   resource,
				ID:        resourceID,
			},
		}},
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("subscriptions: marshal notification: %w", err)
	}
	return b, nil
}

// randomToken returns a 256-bit cryptographically random value as hex, used both
// for subscription IDs and validation tokens.
func randomToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

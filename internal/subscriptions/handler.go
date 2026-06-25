package subscriptions

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// maxSubscriptionBody bounds a POST {base} request body. A subscription is a
// small JSON object (a handful of short string fields), so a few KiB is generous
// while still refusing an oversized body as an amplification guard.
const maxSubscriptionBody = 16 << 10 // 16 KiB

// Handler serves the Microsoft Graph change-notification subscription endpoint
// (https://learn.microsoft.com/graph/api/resources/subscription). It is a
// self-contained stdlib http.Handler the caller mounts under a base path (e.g.
// "/v1.0/subscriptions"); the /subscriptions surface is not part of the
// generated Graph API, so this handler stands alone the way [internal/batch]
// does.
//
// It routes:
//
//   - POST   {base}        create a subscription;
//   - GET    {base}        list subscriptions as {"value":[...]};
//   - DELETE {base}/{id}   delete a subscription;
//
// and renders Graph-shaped error objects ({"error":{"code","message"}}) for
// every rejection.
type Handler struct {
	store            Store
	client           *http.Client
	allowedResources []string
	maxTTL           time.Duration
	now              func() time.Time
	// owner returns the opaque principal key for a request's authenticated
	// identity, stamped onto each created subscription to scope delivery. It is
	// injected (rather than importing internal/auth here) so the package stays
	// decoupled; a nil owner means single-tenant mode — every subscription gets
	// the empty owner. Set via SetOwnerFunc.
	owner func(*http.Request) string
	// onChange, when set, is told the principal key and bearer token whenever a
	// subscription is created or renewed, so the per-principal watch manager can
	// (re)start that principal's watch with a fresh token. Set via OnSubscribe.
	onChange func(r *http.Request, owner string)
}

// NewHandler builds the subscriptions [Handler].
//
// store persists subscriptions. client performs the notificationUrl validation
// handshake ([VerifyNotificationURL]); inject it so tests can pass an httptest
// client (which dials 127.0.0.1) while production passes [GuardedClient], whose
// dialer is SSRF-hardened. allowedResources is the case-insensitive allow-list a
// subscription's resource must match. maxTTL is the largest lifetime a
// subscription may request (expirationDateTime no later than now+maxTTL). now is
// the clock, injected so expiry validation is deterministic in tests; a nil now
// defaults to time.Now.
//
// The returned *Handler is safe for concurrent use (the [Store] is) and
// implements http.Handler. Multi-tenant scoping is opt-in via SetOwnerFunc and
// SetOnSubscribe; without them the handler runs in single-tenant mode (empty
// owner, no watch-manager callback).
func NewHandler(store Store, client *http.Client, allowedResources []string, maxTTL time.Duration, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{
		store:            store,
		client:           client,
		allowedResources: allowedResources,
		maxTTL:           maxTTL,
		now:              now,
	}
}

// SetOwnerFunc installs the principal-key extractor used to stamp each created
// subscription's Owner (scoping delivery to that principal). owner is called per
// create/renew request; returning "" leaves the subscription unowned. Call
// before serving. A nil owner restores single-tenant mode.
func (h *Handler) SetOwnerFunc(owner func(*http.Request) string) {
	h.owner = owner
}

// SetOnSubscribe installs the callback invoked after a subscription is created
// or renewed, with the request (carrying the principal's bearer token) and the
// owner key, so the per-principal watch manager can (re)start that principal's
// watch with the fresh token. Call before serving.
func (h *Handler) SetOnSubscribe(fn func(r *http.Request, owner string)) {
	h.onChange = fn
}

// ownerOf returns the principal key for r, or "" in single-tenant mode.
func (h *Handler) ownerOf(r *http.Request) string {
	if h.owner == nil {
		return ""
	}
	return h.owner(r)
}

// notifyWatch tells the watch manager (if installed) that owner created or
// renewed a subscription on r, so it can refresh that principal's watch.
func (h *Handler) notifyWatch(r *http.Request, owner string) {
	if h.onChange != nil {
		h.onChange(r, owner)
	}
}

// subscriptionWire is the JSON shape of a Graph subscription on the wire. The
// @odata.* envelope fields are omitted for now; expirationDateTime is RFC3339.
type subscriptionWire struct {
	ID                       string `json:"id,omitempty"`
	ChangeType               string `json:"changeType"`
	NotificationURL          string `json:"notificationUrl"`
	LifecycleNotificationURL string `json:"lifecycleNotificationUrl,omitempty"`
	Resource                 string `json:"resource"`
	ExpirationDateTime       string `json:"expirationDateTime"`
	ClientState              string `json:"clientState,omitempty"`
}

// toWire renders a stored Subscription as its wire shape. clientState IS echoed
// back: Graph returns it on create so the subscriber can confirm what was stored.
func toWire(sub Subscription) subscriptionWire {
	return subscriptionWire{
		ID:                       sub.ID,
		ChangeType:               string(sub.ChangeType),
		NotificationURL:          sub.NotificationURL,
		LifecycleNotificationURL: sub.LifecycleNotificationURL,
		Resource:                 sub.Resource,
		ExpirationDateTime:       sub.ExpirationDateTime.UTC().Format(time.RFC3339),
		ClientState:              sub.ClientState,
	}
}

// listEnvelope wraps a subscription list as Graph's {"value":[...]} collection.
type listEnvelope struct {
	Value []subscriptionWire `json:"value"`
}

// ServeHTTP routes by method and path. The handler is mount-agnostic: it locates
// its own "subscriptions" path segment (wherever the caller mounts it, e.g.
// "/v1.0/subscriptions") and treats any single segment after it as the
// subscription id. POST and GET target the collection (no id); DELETE targets a
// single id below it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := subscriptionID(r.URL.Path)

	switch r.Method {
	case http.MethodPost:
		if id != "" {
			writeError(w, http.StatusNotFound, "notFound", "The requested resource does not exist.")
			return
		}
		h.create(w, r)
	case http.MethodGet:
		if id != "" {
			writeError(w, http.StatusNotFound, "notFound", "The requested resource does not exist.")
			return
		}
		h.list(w, r)
	case http.MethodPatch:
		if id == "" {
			writeError(w, http.StatusNotFound, "notFound", "A subscription id is required.")
			return
		}
		h.renew(w, r, id)
	case http.MethodDelete:
		if id == "" {
			writeError(w, http.StatusNotFound, "notFound", "A subscription id is required.")
			return
		}
		h.delete(w, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "methodNotAllowed", "The HTTP method is not allowed.")
	}
}

// subscriptionID extracts the subscription id from a request path, independent
// of where the handler is mounted. It finds the last "subscriptions" path
// segment and returns the single segment after it (empty for the collection
// endpoint, the id for an item endpoint). A path with more than one segment after
// "subscriptions" yields an empty id, which the item routes reject as not-found.
func subscriptionID(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] == "subscriptions" {
			rest := segments[i+1:]
			if len(rest) == 1 {
				return rest[0]
			}
			return ""
		}
	}
	// No "subscriptions" segment found (unconventional mount): fall back to the
	// final non-empty segment so a bare "/{id}" mount still resolves an id.
	if n := len(segments); n > 0 && segments[n-1] != "" {
		return segments[n-1]
	}
	return ""
}

// create handles POST {base}: decode, validate, run the notificationUrl
// handshake, persist, and respond 201 with the stored subscription.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSubscriptionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var wire subscriptionWire
	if err := dec.Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalidRequest", "The subscription request body is not valid JSON.")
		return
	}

	owner := h.ownerOf(r)
	// In multi-tenant mode (an owner extractor is installed) refuse to store a
	// subscription we cannot attribute to a principal: an empty owner would land
	// it in the single-tenant bucket, where single-tenant delivery could leak
	// other principals' notifications to it. Fail closed.
	if h.owner != nil && owner == "" {
		writeError(w, http.StatusForbidden, "accessDenied",
			"The authenticated identity cannot be mapped to a subscription owner.")
		return
	}

	sub := Subscription{
		Resource:                 wire.Resource,
		ChangeType:               ChangeType(wire.ChangeType),
		NotificationURL:          wire.NotificationURL,
		LifecycleNotificationURL: wire.LifecycleNotificationURL,
		ClientState:              wire.ClientState,
		Owner:                    owner,
	}
	if wire.ExpirationDateTime != "" {
		exp, err := time.Parse(time.RFC3339, wire.ExpirationDateTime)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalidRequest",
				"expirationDateTime must be an RFC3339 date-time.")
			return
		}
		sub.ExpirationDateTime = exp
	}

	now := h.now()
	if err := Validate(sub, now, h.maxTTL, h.allowedResources); err != nil {
		writeError(w, http.StatusBadRequest, validationCode(err), err.Error())
		return
	}

	// Prove the client owns the notificationUrl before storing anything: it must
	// echo the validationToken we POST it.
	if err := VerifyNotificationURL(r.Context(), h.client, sub.NotificationURL); err != nil {
		writeError(w, http.StatusBadRequest, "invalidRequest",
			"The notificationUrl did not pass the validation handshake.")
		return
	}

	// A lifecycleNotificationUrl is optional, but when present it must be https
	// and pass the same ownership handshake — it receives reauthorizationRequired
	// and missed events, so an unverified URL is the same exposure as an
	// unverified notificationUrl.
	if sub.LifecycleNotificationURL != "" {
		if !strings.HasPrefix(sub.LifecycleNotificationURL, "https://") {
			writeError(w, http.StatusBadRequest, "invalidRequest",
				"lifecycleNotificationUrl must be an https URL.")
			return
		}
		if err := VerifyNotificationURL(r.Context(), h.client, sub.LifecycleNotificationURL); err != nil {
			writeError(w, http.StatusBadRequest, "invalidRequest",
				"The lifecycleNotificationUrl did not pass the validation handshake.")
			return
		}
	}

	stored, err := h.store.Create(sub)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generalException",
			"The subscription could not be stored.")
		return
	}

	// Hand the manager the principal + this request's bearer token so it can
	// start (or refresh) the principal's watch. Done after a successful store so
	// a watch is only started for a subscription that exists.
	h.notifyWatch(r, stored.Owner)

	writeJSON(w, http.StatusCreated, toWire(stored))
}

// renew handles PATCH {base}/{id}: extend a subscription's expirationDateTime,
// the Graph renewal operation. It enforces ownership (a principal may only renew
// its own subscription — a mismatch is reported as not-found so the handler does
// not reveal another principal's subscription), validates the new expiry against
// the same bounds as create, updates the store, and re-runs the watch callback
// so the manager refreshes the principal's watch with this request's fresh token.
func (h *Handler) renew(w http.ResponseWriter, r *http.Request, id string) {
	existing, err := h.store.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "notFound", "The requested subscription does not exist.")
		return
	}
	if existing.Owner != h.ownerOf(r) {
		// Not the owner: hide the subscription's existence.
		writeError(w, http.StatusNotFound, "notFound", "The requested subscription does not exist.")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSubscriptionBody)
	var wire subscriptionWire
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalidRequest", "The subscription request body is not valid JSON.")
		return
	}
	if wire.ExpirationDateTime == "" {
		writeError(w, http.StatusBadRequest, "invalidRequest", "expirationDateTime is required to renew a subscription.")
		return
	}
	exp, err := time.Parse(time.RFC3339, wire.ExpirationDateTime)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalidRequest", "expirationDateTime must be an RFC3339 date-time.")
		return
	}

	now := h.now()
	if !exp.After(now) {
		writeError(w, http.StatusBadRequest, "invalidRequest", ErrExpirationInPast.Error())
		return
	}
	if exp.After(now.Add(h.maxTTL)) {
		writeError(w, http.StatusBadRequest, "invalidRequest", ErrExpirationTooFar.Error())
		return
	}

	updated, err := h.store.Renew(id, exp)
	if err != nil {
		writeError(w, http.StatusNotFound, "notFound", "The requested subscription does not exist.")
		return
	}

	// Refresh the principal's watch with this request's bearer token — the whole
	// point of renewal-driven push: each renewal re-arms the watch.
	h.notifyWatch(r, updated.Owner)

	writeJSON(w, http.StatusOK, toWire(updated))
}

// list handles GET {base}: render the caller's subscriptions as {"value":[...]}.
// In multi-tenant mode (an owner extractor is installed) it returns only the
// requesting principal's subscriptions, so one principal cannot enumerate
// another's; in single-tenant mode it returns the whole (unowned) store.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	var subs []Subscription
	if h.owner != nil {
		subs = h.store.ListByOwner(h.ownerOf(r))
	} else {
		subs = h.store.List()
	}
	out := listEnvelope{Value: make([]subscriptionWire, 0, len(subs))}
	for _, sub := range subs {
		out.Value = append(out.Value, toWire(sub))
	}
	writeJSON(w, http.StatusOK, out)
}

// delete handles DELETE {base}/{id}: 204 on success, 404 when absent.
func (h *Handler) delete(w http.ResponseWriter, id string) {
	if err := h.store.Delete(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "notFound", "The requested subscription does not exist.")
			return
		}
		writeError(w, http.StatusInternalServerError, "generalException",
			"The subscription could not be deleted.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validationCode maps a [Validate] sentinel error to the Graph error "code" a
// 400 should carry. Every sentinel is a malformed-request variant, so the codes
// stay in the invalidRequest/badRequest family while naming the specific fault.
func validationCode(err error) string {
	switch {
	case errors.Is(err, ErrNotificationURLRequired),
		errors.Is(err, ErrNotificationURLNotHTTPS):
		return "invalidRequest"
	case errors.Is(err, ErrInvalidChangeType):
		return "invalidRequest"
	case errors.Is(err, ErrUnsupportedResource):
		return "invalidRequest"
	case errors.Is(err, ErrExpirationInPast),
		errors.Is(err, ErrExpirationTooFar):
		return "invalidRequest"
	default:
		return "badRequest"
	}
}

// writeJSON renders v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders the Graph error-object shape ({"error":{"code","message"}}),
// matching the wire format produced by [internal/grapherr] and [internal/batch].
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {
			"code":    code,
			"message": message,
		},
	})
}

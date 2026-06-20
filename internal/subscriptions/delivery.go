package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// deliveryTimeout bounds a single change-notification POST so one hung or slow
// subscriber cannot stall the whole fan-out. Graph itself is generous about how
// long a subscriber may take to accept a notification, but the delivery loop
// must keep moving, so each POST gets its own short deadline.
const deliveryTimeout = 10 * time.Second

// Change describes one logical change to a watched resource that the delivery
// loop (driven by IMAP IDLE in a follow-up) hands to [Notify]. Resource is the
// resource path that changed (e.g. "/me/messages"), ChangeType is the kind of
// change (a single kind, matched against each subscription's comma-separated
// changeType list), and ResourceIDs are the specific item ids the change
// touched — all delivered to one subscription in a single batched POST.
type Change struct {
	Resource    string
	ChangeType  ChangeType
	ResourceIDs []string
}

// Result reports the outcome of a [Notify] fan-out. Matched counts the
// subscriptions whose resource/changeType/expiry made them eligible for this
// change; Delivered counts those that accepted the POST with a 2xx; Errors
// collects one entry per subscription that failed (transport error or non-2xx),
// keyed by subscription ID, so the caller can log failures without the fan-out
// having aborted.
type Result struct {
	Matched   int
	Delivered int
	Errors    map[string]error
}

// Notify delivers one logical change to every subscription that watches it.
//
// A subscription matches change when all of the following hold:
//
//   - its Resource equals change.Resource (case-insensitive, matching the
//     resource allow-list comparison used elsewhere in this package);
//   - its (comma-separated) ChangeType list includes change.ChangeType;
//   - it is not expired — ExpirationDateTime is strictly after now.
//
// For each matching subscription, Notify builds ONE change-notification envelope
// covering every id in change.ResourceIDs (the same per-entry shape as
// [NotificationPayload], clientState echoed) and POSTs it to the subscription's
// NotificationURL. Each POST is bounded by a short timeout derived from ctx
// (see [deliveryTimeout]).
//
// Delivery is best-effort and isolated per subscription: a transport error or a
// non-2xx response is recorded in Result.Errors and does not stop delivery to
// the other subscriptions. now is injected so expiry filtering is deterministic
// in tests. client must be non-nil; production passes [GuardedClient], whose
// dialer is SSRF-hardened (NotificationURL is client-supplied).
func Notify(ctx context.Context, client *http.Client, store Store, change Change, now time.Time) Result {
	result := Result{Errors: make(map[string]error)}

	for _, sub := range store.List() {
		if !subscriptionMatches(sub, change, now) {
			continue
		}
		result.Matched++

		if err := deliver(ctx, client, sub, change.ResourceIDs); err != nil {
			result.Errors[sub.ID] = err
			continue
		}
		result.Delivered++
	}
	return result
}

// subscriptionMatches reports whether sub should receive change as of now: the
// resource matches, the change kind is one the subscription asked for, and the
// subscription has not expired.
func subscriptionMatches(sub Subscription, change Change, now time.Time) bool {
	if !strings.EqualFold(sub.Resource, change.Resource) {
		return false
	}
	if !changeTypeIncludes(sub.ChangeType, change.ChangeType) {
		return false
	}
	return sub.ExpirationDateTime.After(now)
}

// changeTypeIncludes reports whether the comma-separated list have contains the
// single change kind want (e.g. "created,updated" includes "updated").
// Whitespace around each token is trimmed to match [validateChangeType].
func changeTypeIncludes(have, want ChangeType) bool {
	for _, part := range strings.Split(string(have), ",") {
		if ChangeType(strings.TrimSpace(part)) == want {
			return true
		}
	}
	return false
}

// deliver POSTs a single batched change-notification covering resourceIDs to
// sub.NotificationURL, bounded by a per-request timeout. A nil client, a
// transport error, or a non-2xx response is returned as an error so [Notify] can
// record it as a per-subscription failure.
func deliver(ctx context.Context, client *http.Client, sub Subscription, resourceIDs []string) error {
	if client == nil {
		return fmt.Errorf("subscriptions: nil http client")
	}

	body, err := NotificationPayloadBatch(sub, resourceIDs)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.NotificationURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("subscriptions: build notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("subscriptions: notification request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("subscriptions: notification returned status %d", resp.StatusCode)
	}
	return nil
}

// NotificationPayloadBatch builds the JSON change-notification envelope for sub
// covering several changed item ids in a single POST body: one "value" entry per
// id, each with the same shape as [NotificationPayload] (resource = sub.Resource
// with the id appended, clientState echoed). The delivery loop POSTs the returned
// bytes to sub.NotificationURL. An empty resourceIDs yields an empty
// {"value":[]} envelope.
func NotificationPayloadBatch(sub Subscription, resourceIDs []string) ([]byte, error) {
	base := strings.TrimRight(sub.Resource, "/")
	entries := make([]changeNotification, 0, len(resourceIDs))
	for _, id := range resourceIDs {
		resource := base + "/" + id
		entries = append(entries, changeNotification{
			SubscriptionID: sub.ID,
			ClientState:    sub.ClientState,
			ChangeType:     sub.ChangeType,
			Resource:       resource,
			ResourceData: resourceData{
				ODataType: "#Microsoft.Graph.Message",
				ODataID:   resource,
				ID:        id,
			},
		})
	}
	b, err := json.Marshal(notificationEnvelope{Value: entries})
	if err != nil {
		return nil, fmt.Errorf("subscriptions: marshal batch notification: %w", err)
	}
	return b, nil
}

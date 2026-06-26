package subscriptions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// LifecycleEvent is the kind of lifecycle notification Graph POSTs to a
// subscription's lifecycleNotificationUrl, out of band from data notifications.
type LifecycleEvent string

const (
	// LifecycleReauthorizationRequired asks the subscriber to reauthorize the
	// subscription — re-present a fresh token (PATCH renew) — because the
	// authorization backing delivery is about to lapse. It is the proactive
	// signal that keeps a renewal-driven watch alive across token expiry.
	LifecycleReauthorizationRequired LifecycleEvent = "reauthorizationRequired"
	// LifecycleMissed tells the subscriber that delivery has gaps and it should
	// reconcile via a delta query. The watch lapsed (e.g. the token expired
	// before reauthorization) so changes may not have been pushed.
	LifecycleMissed LifecycleEvent = "missed"
	// LifecycleSubscriptionRemoved tells the subscriber its subscription is gone
	// (e.g. it expired) and must be recreated.
	LifecycleSubscriptionRemoved LifecycleEvent = "subscriptionRemoved"
)

// lifecycleNotification is one entry in a lifecycle payload's "value" array,
// matching the shape Graph POSTs to a lifecycleNotificationUrl.
type lifecycleNotification struct {
	SubscriptionID                 string         `json:"subscriptionId"`
	SubscriptionExpirationDateTime string         `json:"subscriptionExpirationDateTime,omitempty"`
	LifecycleEvent                 LifecycleEvent `json:"lifecycleEvent"`
	Resource                       string         `json:"resource"`
	ClientState                    string         `json:"clientState,omitempty"`
}

// lifecycleEnvelope wraps the lifecycle notifications POSTed to a
// lifecycleNotificationUrl.
type lifecycleEnvelope struct {
	Value []lifecycleNotification `json:"value"`
}

// NotifyLifecycle POSTs a lifecycle event to the lifecycleNotificationUrl of
// every subscription owned by owner that has one configured and has not expired.
// It mirrors [Notify]: each POST gets its own timeout, a failure is recorded
// per-subscription without aborting the rest, and the Result tallies the
// outcome. owner "" addresses the single-tenant (unowned) subscriptions.
func NotifyLifecycle(ctx context.Context, client *http.Client, store Store, owner string, event LifecycleEvent, now time.Time) Result {
	result := Result{Errors: make(map[string]error)}
	for _, sub := range store.ListByOwner(owner) {
		if sub.LifecycleNotificationURL == "" {
			continue
		}
		if !sub.ExpirationDateTime.After(now) {
			continue
		}
		result.Matched++
		if err := deliverLifecycle(ctx, client, sub, event); err != nil {
			result.Errors[sub.ID] = err
			continue
		}
		result.Delivered++
	}
	return result
}

// deliverLifecycle POSTs a single lifecycle notification for sub to its
// lifecycleNotificationUrl, bounded by the same per-request timeout as a data
// notification. A nil client, a transport error, or a non-2xx response is an
// error the caller records per-subscription.
func deliverLifecycle(ctx context.Context, client *http.Client, sub Subscription, event LifecycleEvent) error {
	if client == nil {
		return fmt.Errorf("subscriptions: nil http client")
	}
	body, err := json.Marshal(lifecycleEnvelope{Value: []lifecycleNotification{{
		SubscriptionID:                 sub.ID,
		SubscriptionExpirationDateTime: sub.ExpirationDateTime.UTC().Format(time.RFC3339),
		LifecycleEvent:                 event,
		Resource:                       sub.Resource,
		ClientState:                    sub.ClientState,
	}}})
	if err != nil {
		return fmt.Errorf("subscriptions: marshal lifecycle notification: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.LifecycleNotificationURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("subscriptions: build lifecycle request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("subscriptions: lifecycle request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("subscriptions: lifecycle returned status %d", resp.StatusCode)
	}
	return nil
}

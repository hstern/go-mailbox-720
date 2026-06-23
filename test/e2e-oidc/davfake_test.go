// davfake_test.go is an in-process CalDAV server for the impersonation e2e. It is
// the resource-server end of the chain: mailboxd, having exchanged a user's token
// for a CalDAV-audience token, dials this server with that opaque exchanged token
// as its bearer. The fake introspects the bearer via the same RFC 7662 validator a
// real backend would (tokenValidator), resolves the subject, and serves that
// subject's calendar events out of the shared userStore — so a request for userA's
// events can never return userB's.
//
// It wraps the emersion/go-webdav caldav server Handler (which generates exactly
// the PROPFIND/REPORT multistatus XML the real caldav client decodes) behind a
// Bearer-auth middleware. The middleware validates each request's bearer, resolves
// the sub, and stashes it on the request context; the caldav Backend then reads
// that sub and synthesises one default calendar of VEVENTs from store.events(sub).
// The CalDAV client exercises this surface for GET /me/events:
//
//   - PROPFIND "/" (current-user-principal)         -> the fixed principal path
//   - PROPFIND <principal> (calendar-home-set)      -> the fixed home-set path
//   - PROPFIND <home-set> Depth:1 (calendar list)   -> the single default calendar
//   - REPORT <calendar> calendar-query              -> the subject's events as VEVENTs
//
// The per-sub data is synthesised on each request, so the fake holds no per-user
// state of its own. The bearer token is never logged.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

// Fixed WebDAV paths the fake advertises. The CalDAV client discovers the
// principal, then its calendar-home-set, then the calendars within; a single
// default calendar makes the subject's seeded events the ones /me/events returns.
//
// The go-webdav caldav server classifies a request's resource type by counting
// path segments (server.go resourceTypeAtPath): 1 = user-principal, 2 =
// calendar-home-set, 3 = calendar, 4 = calendar-object. These paths are nested to
// match that scheme so each PROPFIND is dispatched to the right handler — only the
// user-principal handler returns the calendar-home-set property the client needs.
const (
	davPrincipalPath = "/me/"
	davHomeSetPath   = "/me/cal/"
	davCalendarPath  = "/me/cal/default/"
	davCalendarName  = "Calendar"
)

// davSubKey types the context key under which the auth middleware stashes the
// validated subject for the caldav Backend to read.
type davSubKey struct{}

// startCalDAVFake wires the caldav Backend behind a Bearer-auth middleware into an
// httptest.Server and returns its base URL (the CalDAV endpoint mailboxd dials). v
// introspects the bearer for the CalDAV audience and yields the subject; store
// supplies that subject's seeded events.
func startCalDAVFake(t *testing.T, v *tokenValidator, store *userStore) (url string) {
	t.Helper()
	handler := &caldav.Handler{Backend: &davBackend{store: store}}

	srv := httptest.NewServer(davAuthMiddleware(v, handler))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// davAuthMiddleware extracts the Authorization: Bearer token, introspects it via v,
// and on success stashes the resolved subject on the request context before
// delegating to next. On any failure it writes 401 and stops. The token itself is
// never logged. It mirrors internal/davauth's Bearer posture on the server side.
func davAuthMiddleware(v *tokenValidator, next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		tok := strings.TrimSpace(h[len(prefix):])
		sub, err := v.validate(tok)
		if err != nil {
			http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), davSubKey{}, sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// davBackend implements caldav.Backend over the shared store. It is stateless: the
// subject comes from the request context (set by davAuthMiddleware), so every
// method serves strictly that subject's data. Only the read paths /me/events
// exercises are real; the write/delete paths return an error.
type davBackend struct {
	store *userStore
}

// subFromContext returns the validated subject the middleware stashed. An empty
// subject (impossible after the middleware, but defended here) yields no events
// rather than another user's.
func subFromContext(ctx context.Context) string {
	sub, _ := ctx.Value(davSubKey{}).(string)
	return sub
}

func (b *davBackend) CurrentUserPrincipal(_ context.Context) (string, error) {
	return davPrincipalPath, nil
}

func (b *davBackend) CalendarHomeSetPath(_ context.Context) (string, error) {
	return davHomeSetPath, nil
}

// theCalendar is the single default calendar the fake exposes for every subject.
func theCalendar() caldav.Calendar {
	return caldav.Calendar{
		Path:                  davCalendarPath,
		Name:                  davCalendarName,
		SupportedComponentSet: []string{ical.CompEvent},
	}
}

func (b *davBackend) ListCalendars(_ context.Context) ([]caldav.Calendar, error) {
	return []caldav.Calendar{theCalendar()}, nil
}

func (b *davBackend) GetCalendar(_ context.Context, _ string) (*caldav.Calendar, error) {
	c := theCalendar()
	return &c, nil
}

// calendarObjects synthesises the subject's seeded events as CalDAV calendar
// objects, each a VCALENDAR wrapping one VEVENT whose SUMMARY is the event's
// Subject. Object paths are positional and stable for one request, which is all
// the list path (calendar-query REPORT) needs.
func (b *davBackend) calendarObjects(ctx context.Context) ([]caldav.CalendarObject, error) {
	sub := subFromContext(ctx)
	evs := b.store.events(sub)
	out := make([]caldav.CalendarObject, 0, len(evs))
	for i, e := range evs {
		cal, err := eventCalendar(i, e)
		if err != nil {
			return nil, err
		}
		out = append(out, caldav.CalendarObject{
			Path: fmt.Sprintf("%sevent-%d.ics", davCalendarPath, i),
			Data: cal,
		})
	}
	return out, nil
}

func (b *davBackend) ListCalendarObjects(ctx context.Context, _ string, _ *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	return b.calendarObjects(ctx)
}

func (b *davBackend) QueryCalendarObjects(ctx context.Context, _ string, _ *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	return b.calendarObjects(ctx)
}

func (b *davBackend) GetCalendarObject(ctx context.Context, path string, _ *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	objs, err := b.calendarObjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range objs {
		if objs[i].Path == path {
			return &objs[i], nil
		}
	}
	return nil, fmt.Errorf("caldav fake: calendar object %q not found", path)
}

// The write surface is unused by /me/events; reject it loudly so a stray write
// cannot silently succeed.
func (b *davBackend) CreateCalendar(context.Context, *caldav.Calendar) error {
	return fmt.Errorf("caldav fake: CreateCalendar unsupported")
}

func (b *davBackend) PutCalendarObject(context.Context, string, *ical.Calendar, *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	return nil, fmt.Errorf("caldav fake: PutCalendarObject unsupported")
}

func (b *davBackend) DeleteCalendarObject(context.Context, string) error {
	return fmt.Errorf("caldav fake: DeleteCalendarObject unsupported")
}

// eventCalendar builds a minimal valid VCALENDAR for one seeded event: a single
// VEVENT carrying a stable UID, the required DTSTAMP/DTSTART (RFC 5545), and the
// event's Subject as SUMMARY. The times are fixed and unimportant — /me/events
// only asserts the subject — but must be present and UTC so the client maps them.
func eventCalendar(i int, e event) (*ical.Calendar, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, "-//go-mailbox-720//caldav-e2e-fake//EN")

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, fmt.Sprintf("evt-%d@go-mailbox-720.test", i))
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, stamp)
	start := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
	ev.Props.SetText(ical.PropSummary, e.Subject)
	cal.Children = append(cal.Children, ev.Component)

	// Round-trip through the encoder to validate the object is well-formed before
	// the server hands it to the client (a malformed VEVENT would fail the client's
	// ical decode with an opaque error).
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("caldav fake: encode event %d: %w", i, err)
	}
	return cal, nil
}

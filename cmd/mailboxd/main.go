// Command mailboxd runs the Microsoft Graph mailbox server.
//
// It serves the generated Graph mailbox API under /v1.0. Every operation is
// currently "not implemented" (the server skeleton, MB720-3); operations are
// filled in by later issues.
//
// When one or more -auth-issuer URLs are configured, every request must carry a
// valid bearer JWT from a trusted issuer (the OIDC resource-server posture,
// MB720-4). With no issuer configured, auth is disabled and all requests are
// allowed — convenient for the anonymous conformance harness, never for
// production.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	// Embed the IANA time-zone database so event time-zone resolution
	// (internal/tz) works regardless of the host's system zoneinfo.
	_ "time/tzdata"

	clientauthn "github.com/hstern/go-oauth-client-authn"
	"github.com/hstern/go-ssf/receiver"

	"github.com/hstern/go-mailbox-720/internal/auth"
	"github.com/hstern/go-mailbox-720/internal/batch"
	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/calendar/caldav"
	calendarjmap "github.com/hstern/go-mailbox-720/internal/calendar/jmap"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/contacts/carddav"
	jmapcontacts "github.com/hstern/go-mailbox-720/internal/contacts/jmap"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mail/imap"
	mailjmap "github.com/hstern/go-mailbox-720/internal/mail/jmap"
	"github.com/hstern/go-mailbox-720/internal/notify"
	"github.com/hstern/go-mailbox-720/internal/revocation"
	"github.com/hstern/go-mailbox-720/internal/schedrun"
	"github.com/hstern/go-mailbox-720/internal/server"
	"github.com/hstern/go-mailbox-720/internal/smtp"
	"github.com/hstern/go-mailbox-720/internal/subscriptions"
	"github.com/hstern/go-mailbox-720/internal/tokenexchange"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	issuers := flag.String("auth-issuer", "", "comma-separated trusted OIDC issuer URLs (empty disables auth)")
	audience := flag.String("auth-audience", "", "expected token audience (aud)")
	scopes := flag.String("auth-scope", "", "comma-separated required scopes")
	subjectClaim := flag.String("auth-subject-claim", "sub", "token claim mapped to the mailbox identity")
	scopeClaims := flag.String("auth-scope-claims", "scope,roles", "comma-separated claims that carry granted scopes (Microsoft Entra/Azure AD: \"scope,scp,roles\" — scp is Entra's non-standard delegated-scope claim)")
	introspectID := flag.String("auth-introspect-client-id", "", "OAuth2 client id for RFC 7662 introspection of opaque tokens (enables introspection; secret from MAILBOXD_INTROSPECT_CLIENT_SECRET)")
	resourceID := flag.String("auth-resource", "", "this resource's identifier URL (RFC 8707); when set, publishes RFC 9728 protected-resource metadata at /.well-known/oauth-protected-resource")
	tokenExchangeEndpoint := flag.String("tokenexchange-endpoint", "", "OAuth2 token endpoint for RFC 8693 token exchange (empty disables). Enables per-identity backends (MB720-41): a backend provider exchanges the authenticated user's token for a backend-audience token preserving the user's sub")
	tokenExchangeClientID := flag.String("tokenexchange-client-id", "", "OAuth2 client id mailboxd authenticates with at the token-exchange endpoint (required when -tokenexchange-endpoint is set; secret from MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET)")
	tokenExchangeClientAuth := flag.String("tokenexchange-client-auth", "client_secret_basic", "client-authentication method at the token-exchange endpoint: client_secret_basic or client_secret_post")
	ssfReceiverPath := flag.String("ssf-receiver-path", "/ssf/events", "public path for the Shared Signals SET receiver (CAEP/RISC revocation, MB720-18); served unauthenticated since the SET signature is the gate. Active only when auth is enabled")
	imapAddr := flag.String("mail-imap-addr", "", "IMAP server address host:port for the mail backend (empty: mail operations return 501; password from MAILBOXD_IMAP_PASSWORD)")
	imapUser := flag.String("mail-imap-username", "", "IMAP username for the mail backend")
	imapTLS := flag.Bool("mail-imap-tls", true, "use implicit TLS for the IMAP connection")
	sieveAddr := flag.String("mail-managesieve-addr", "", "ManageSieve server host:port (RFC 5804) for inbox rules on the IMAP tier (empty: mail-filter operations return 501; SASL PLAIN reuses the IMAP username and MAILBOXD_IMAP_PASSWORD)")
	sieveTLS := flag.Bool("mail-managesieve-starttls", true, "use STARTTLS on the ManageSieve connection (RFC 5804 §2.2); disabling it sends the SASL PLAIN credentials in the clear — for local servers only")
	jmapSession := flag.String("mail-jmap-session-url", "", "JMAP session resource URL for the mail backend (empty: use IMAP, or return 501 if neither set; access token from MAILBOXD_JMAP_TOKEN). Takes precedence over the IMAP flags when set")
	mailJMAPAudience := flag.String("mail-jmap-audience", "", "RFC 8693 audience to request for the JMAP mail backend (MB720-42). When set together with -tokenexchange-endpoint, the mail backend authenticates per-identity — exchanging the user's token for a backend-audience token preserving their sub — instead of using the shared MAILBOXD_JMAP_TOKEN")
	caldavURL := flag.String("cal-caldav-url", "", "CalDAV base URL for the calendar backend (empty: calendar operations return 501; password from MAILBOXD_CALDAV_PASSWORD). Use an https:// URL for TLS")
	caldavUser := flag.String("cal-caldav-username", "", "CalDAV username for the calendar backend")
	jmapCalSession := flag.String("cal-jmap-session-url", "", "JMAP session resource URL for the calendar backend (empty: use CalDAV, or return 501 if neither set; access token from MAILBOXD_CALENDAR_JMAP_TOKEN). Takes precedence over the CalDAV flags when set")
	jmapCalUsername := flag.String("cal-jmap-username", "", "JMAP calendar username for HTTP Basic auth (empty: use bearer token from MAILBOXD_CALENDAR_JMAP_TOKEN; password from MAILBOXD_CALENDAR_JMAP_PASSWORD)")
	jmapCalAPIURL := flag.String("cal-jmap-api-url", "", "override the apiUrl advertised in the JMAP session document (for servers advertising an unreachable internal apiUrl)")
	carddavURL := flag.String("contacts-carddav-url", "", "CardDAV base URL for the contacts backend (empty: contacts operations return 501; password from MAILBOXD_CARDDAV_PASSWORD). Use an https:// URL for TLS")
	carddavUser := flag.String("contacts-carddav-username", "", "CardDAV username for the contacts backend")
	contactsJMAP := flag.String("contacts-jmap-session-url", "", "JMAP session resource URL for the contacts backend (empty: use CardDAV, or return 501 if neither set; access token from MAILBOXD_CONTACTS_JMAP_TOKEN). Takes precedence over the CardDAV flags when set")
	contactsJMAPAudience := flag.String("contacts-jmap-audience", "", "RFC 8693 audience to request for the JMAP contacts backend (MB720-42). When set together with -tokenexchange-endpoint, the contacts backend authenticates per-identity instead of using the shared MAILBOXD_CONTACTS_JMAP_TOKEN")
	enableScheduling := flag.Bool("enable-scheduling", false, "run the iTIP scheduling trigger: turn inbound mail invitations into tentative calendar events (needs IMAP + CalDAV backends; opt-in, since it writes to the calendar). Auto-disables when the CalDAV server schedules natively (RFC 6638)")
	smtpAddr := flag.String("smtp-addr", "", "SMTP submission server host:port for emailing meeting accept/decline replies (empty: those operations return 501; password from MAILBOXD_SMTP_PASSWORD)")
	smtpUser := flag.String("smtp-username", "", "SMTP username")
	smtpTLS := flag.Bool("smtp-tls", false, "use implicit TLS (SMTPS, port 465) for SMTP")
	smtpStartTLS := flag.Bool("smtp-starttls", true, "use STARTTLS (submission, port 587) for SMTP; ignored when -smtp-tls is set")
	mailboxEmail := flag.String("mailbox-email", "", "the mailbox owner's email, used as the responding attendee when accepting/declining meetings")
	flag.Parse()

	// The revocation store is the receiver's sink AND the middleware's revocation
	// checker: a SET delivered to the receiver mutates it; every authenticated
	// request reads it. One shared instance ties the two halves together.
	revStore := revocation.NewStore()

	cfg := auth.Config{
		Issuers:        splitList(*issuers),
		Audience:       *audience,
		RequiredScopes: splitList(*scopes),
		SubjectClaim:   *subjectClaim,
		ScopeClaims:    splitList(*scopeClaims),
		ResourceID:     *resourceID,
		Revocations:    revStore,
	}
	if *introspectID != "" {
		// The secret is taken from the environment, never a flag, so it does not
		// appear in the process table.
		cfg.Introspection = &auth.IntrospectionConfig{
			ClientID:     *introspectID,
			ClientSecret: os.Getenv("MAILBOXD_INTROSPECT_CLIENT_SECRET"),
		}
	}
	// Build the RFC 8693 token-exchange helper that per-identity backend providers
	// (MB720-42/43/44) use to mint backend-audience tokens from the authenticated
	// user's token. It is nil when no endpoint is configured — the current default,
	// where the static single-credential providers below serve every request.
	exchanger, err := buildExchanger(*tokenExchangeEndpoint, *tokenExchangeClientID, *tokenExchangeClientAuth)
	if err != nil {
		log.Fatalln("mailboxd:", err)
	}
	// exchanger is consumed by the per-identity providers in MB720-42/43/44; for now
	// it is constructed (validating its configuration at startup) and its status logged.
	if exchanger != nil {
		log.Println("token exchange: enabled (RFC 8693) — per-identity backend auth ready")
	} else {
		log.Println("token exchange: disabled — backends use their configured static credentials")
	}
	// A per-identity audience without an exchange endpoint is a misconfiguration:
	// the backend would silently fall back to the shared static token, serving every
	// authenticated user from one account. Fail loudly rather than leak across tenants.
	if exchanger == nil && (*mailJMAPAudience != "" || *contactsJMAPAudience != "") {
		log.Fatalln("mailboxd: -mail-jmap-audience/-contacts-jmap-audience require -tokenexchange-endpoint (per-identity backends need the token exchange)")
	}

	// JMAP and IMAP are alternative mail backends behind the same port; JMAP wins
	// when its session URL is set (it is the closer fit for the JMAP-shaped port).
	var provider server.MailProvider
	switch {
	case *jmapSession != "" && exchanger != nil && *mailJMAPAudience != "":
		// Per-identity (MB720-42): exchange each user's token for a JMAP-mail-
		// audience token and dial as that user. sessionURL is captured for the dial.
		sessionURL := *jmapSession
		provider = jmapMailIdentityProvider{newPerIdentityBackend(exchanger, *mailJMAPAudience,
			func(token string) (mail.Backend, error) { return mailjmap.Dial(sessionURL, token, nil) })}
	case *jmapSession != "":
		provider = staticJMAPProvider{
			sessionURL: *jmapSession,
			// The token is taken from the environment, never a flag, so it does not
			// appear in the process table.
			token: os.Getenv("MAILBOXD_JMAP_TOKEN"),
		}
	case *imapAddr != "":
		provider = staticIMAPProvider{
			addr:      *imapAddr,
			username:  *imapUser,
			password:  os.Getenv("MAILBOXD_IMAP_PASSWORD"),
			tls:       *imapTLS,
			sieveAddr: *sieveAddr,
			sieveTLS:  *sieveTLS,
		}
	}
	// JMAP and CalDAV are alternative calendar backends behind the same port; JMAP
	// wins when its session URL is set (it is the closer fit for the JMAP-shaped port).
	var calProvider server.CalendarProvider
	switch {
	case *jmapCalSession != "":
		calProvider = staticJMAPCalendarProvider{
			sessionURL: *jmapCalSession,
			// The token is taken from the environment, never a flag, so it does not
			// appear in the process table. When username is set the adapter uses Basic
			// auth and the token is ignored.
			token:    os.Getenv("MAILBOXD_CALENDAR_JMAP_TOKEN"),
			username: *jmapCalUsername,
			// The password is taken from the environment, never a flag, so it does not
			// appear in the process table.
			password: os.Getenv("MAILBOXD_CALENDAR_JMAP_PASSWORD"),
			apiURL:   *jmapCalAPIURL,
		}
	case *caldavURL != "":
		calProvider = staticCalDAVProvider{
			url:      *caldavURL,
			username: *caldavUser,
			password: os.Getenv("MAILBOXD_CALDAV_PASSWORD"),
		}
	}
	// JMAP and CardDAV are alternative contacts backends behind the same port; JMAP
	// wins when its session URL is set (it is the closer fit for the JMAP-shaped port).
	var contactsProvider server.ContactsProvider
	switch {
	case *contactsJMAP != "" && exchanger != nil && *contactsJMAPAudience != "":
		// Per-identity (MB720-42), mirroring the JMAP mail backend.
		sessionURL := *contactsJMAP
		contactsProvider = jmapContactsIdentityProvider{newPerIdentityBackend(exchanger, *contactsJMAPAudience,
			func(token string) (contacts.Backend, error) { return jmapcontacts.Dial(sessionURL, token, nil) })}
	case *contactsJMAP != "":
		contactsProvider = staticJMAPContactsProvider{
			sessionURL: *contactsJMAP,
			// The token is taken from the environment, never a flag, so it does not
			// appear in the process table.
			token: os.Getenv("MAILBOXD_CONTACTS_JMAP_TOKEN"),
		}
	case *carddavURL != "":
		contactsProvider = staticCardDAVProvider{
			url:      *carddavURL,
			username: *carddavUser,
			password: os.Getenv("MAILBOXD_CARDDAV_PASSWORD"),
		}
	}
	var schedProvider server.SchedulingProvider
	if *smtpAddr != "" {
		schedProvider = staticSchedulingProvider{
			addr:     *smtpAddr,
			username: *smtpUser,
			password: os.Getenv("MAILBOXD_SMTP_PASSWORD"),
			tls:      *smtpTLS,
			startTLS: *smtpStartTLS,
			mailbox:  *mailboxEmail,
		}
	}
	if err := run(*addr, cfg, revStore, *ssfReceiverPath, provider, calProvider, contactsProvider, schedProvider, *enableScheduling); err != nil {
		log.Fatalln("mailboxd:", err)
	}
}

// staticIMAPProvider serves every request from one configured IMAP account. A
// per-identity provider (mapping the token's mailbox identity to credentials,
// e.g. via Dovecot master users) is future work.
type staticIMAPProvider struct {
	addr, username, password string
	tls                      bool
	// sieveAddr, when set, enables the inbox-rule capability over ManageSieve
	// (RFC 5804) on that address, reusing the IMAP credentials.
	sieveAddr string
	sieveTLS  bool
}

func (p staticIMAPProvider) Mail(_ context.Context) (mail.Backend, error) {
	o := &imap.Options{TLS: p.tls}
	if p.sieveAddr != "" {
		o.ManageSieve = &imap.ManageSieveOptions{Addr: p.sieveAddr, STARTTLS: p.sieveTLS}
	}
	return imap.Dial(p.addr, p.username, p.password, o)
}

// staticJMAPProvider serves every request from one configured JMAP account,
// mirroring staticIMAPProvider. It is the alternative mail backend behind the
// same port; the token is a JMAP bearer access token sourced from the
// environment. A per-identity provider is future work.
type staticJMAPProvider struct {
	sessionURL, token string
}

func (p staticJMAPProvider) Mail(_ context.Context) (mail.Backend, error) {
	return mailjmap.Dial(p.sessionURL, p.token, nil)
}

// staticCalDAVProvider serves every request from one configured CalDAV account.
// A per-identity provider (mapping the token's mailbox identity to credentials)
// is future work, mirroring staticIMAPProvider.
type staticCalDAVProvider struct {
	url, username, password string
}

func (p staticCalDAVProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	return caldav.Dial(p.url, p.username, p.password, nil)
}

// staticJMAPCalendarProvider serves every request from one configured JMAP
// calendar account — the alternative calendar backend behind the same port
// (mirroring the JMAP mail and contacts backends). When username is non-empty
// the adapter uses HTTP Basic auth (username + password from env) and the token
// is ignored; otherwise the token (bearer, from env) is used. apiURL overrides
// the apiUrl advertised in the JMAP session document when set. A per-identity
// provider is future work.
type staticJMAPCalendarProvider struct {
	sessionURL, token          string
	username, password, apiURL string
}

func (p staticJMAPCalendarProvider) Calendar(_ context.Context) (calendar.Backend, error) {
	opts := &calendarjmap.Options{APIURLOverride: p.apiURL}
	if p.username != "" {
		opts.BasicAuth = &calendarjmap.BasicAuthCredentials{Username: p.username, Password: p.password}
	}
	return calendarjmap.Dial(p.sessionURL, p.token, opts)
}

// staticCardDAVProvider serves every request from one configured CardDAV
// account. A per-identity provider (mapping the token's mailbox identity to
// credentials) is future work, mirroring staticIMAPProvider.
type staticCardDAVProvider struct {
	url, username, password string
}

func (p staticCardDAVProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return carddav.Dial(p.url, p.username, p.password, nil)
}

// staticJMAPContactsProvider serves every request from one configured JMAP contacts
// account — the alternative contacts backend behind the same port (mirroring the
// JMAP mail backend). The token is a JMAP bearer access token sourced from the
// environment. A per-identity provider is future work.
type staticJMAPContactsProvider struct {
	sessionURL, token string
}

func (p staticJMAPContactsProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return jmapcontacts.Dial(p.sessionURL, p.token, nil)
}

// staticSchedulingProvider answers meeting accept/decline by emailing the
// organizer from one configured SMTP account and mailbox address. A per-identity
// provider (mapping the token's identity to credentials) is future work.
type staticSchedulingProvider struct {
	addr, username, password, mailbox string
	tls, startTLS                     bool
}

func (p staticSchedulingProvider) Sender(_ context.Context) (smtp.Sender, error) {
	return smtp.Dial(p.addr, p.username, p.password, &smtp.Options{TLS: p.tls, StartTLS: p.startTLS})
}

func (p staticSchedulingProvider) MailboxAddress() string { return p.mailbox }

func run(addr string, authCfg auth.Config, revSink receiver.Sink, ssfReceiverPath string, provider server.MailProvider, calProvider server.CalendarProvider, contactsProvider server.ContactsProvider, schedProvider server.SchedulingProvider, enableScheduling bool) error {
	h, err := server.New(provider, calProvider, contactsProvider, schedProvider)
	if err != nil {
		return err
	}
	switch provider.(type) {
	case jmapMailIdentityProvider:
		log.Println("mail: JMAP backend enabled (per-identity token exchange)")
	case staticJMAPProvider:
		log.Println("mail: JMAP backend enabled")
	case staticIMAPProvider:
		log.Println("mail: IMAP backend enabled")
	default:
		log.Println("mail: no backend configured — mail operations return 501")
	}
	switch p := calProvider.(type) {
	case staticJMAPCalendarProvider:
		if p.username != "" {
			log.Println("calendar: JMAP backend enabled (Basic auth)")
		} else {
			log.Println("calendar: JMAP backend enabled (bearer token)")
		}
	case staticCalDAVProvider:
		log.Println("calendar: CalDAV backend enabled")
	default:
		log.Println("calendar: no backend configured — calendar operations return 501")
	}
	switch contactsProvider.(type) {
	case jmapContactsIdentityProvider:
		log.Println("contacts: JMAP backend enabled (per-identity token exchange)")
	case staticJMAPContactsProvider:
		log.Println("contacts: JMAP backend enabled")
	case staticCardDAVProvider:
		log.Println("contacts: CardDAV backend enabled")
	default:
		log.Println("contacts: no backend configured — contacts operations return 501")
	}

	// basePath is the Graph version segment the api.Server is mounted under.
	// internal/server keeps its own basePath const unexported, so we repeat the
	// literal here (per MB720-7); they must stay in sync.
	const basePath = "/v1.0"

	// Route POST {basePath}/$batch to the JSON batching handler and everything
	// else to the api.Server. The whole mux is wrapped by the auth middleware
	// below, so the outer /$batch request is authenticated and its sub-requests
	// inherit its context (and thus the authenticated mailbox identity).
	mux := http.NewServeMux()
	mux.Handle("POST "+basePath+"/$batch", batch.Handler(h, basePath))

	// /subscriptions is a Graph endpoint not in the generated API; mount the
	// change-notification handler over an in-process store. The SSRF-guarded
	// client is used for the notificationUrl validation handshake. Delivery of
	// notifications (IMAP IDLE -> POST) is future work; this is create/list/delete.
	//
	// SINGLE-TENANT: one shared store, no per-identity keying — fine for the
	// static single-mailbox model, but before multi-mailbox use the store must be
	// keyed on the authenticated subject (with a per-principal subscription cap),
	// else any authenticated caller can list/delete every caller's subscriptions.
	subStore := subscriptions.NewMemoryStore()
	subHandler := subscriptions.NewHandler(
		subStore, subscriptions.GuardedClient(),
		[]string{"/me/messages", "/me/events", "/me/contacts"},
		72*time.Hour, time.Now,
	)
	mux.Handle(basePath+"/subscriptions", subHandler)
	mux.Handle(basePath+"/subscriptions/", subHandler)
	log.Println("subscriptions: endpoint enabled (in-memory store)")

	// The three delta operations are served by custom handlers (mounted ahead of
	// the api.Server) rather than the generated ones, because a delta page's value
	// array mixes full objects with @removed tombstones for deleted items — a shape
	// the generated typed collection cannot carry. Deletions come from the CalDAV/
	// CardDAV sync-collection and, for mail, IMAP QRESYNC VANISHED.
	mux.Handle("GET "+basePath+"/me/messages/delta()", server.MessagesDeltaHandler(provider))
	mux.Handle("GET "+basePath+"/me/events/delta()", server.EventsDeltaHandler(calProvider))
	mux.Handle("GET "+basePath+"/me/contacts/delta()", server.ContactsDeltaHandler(contactsProvider))

	mux.Handle("/", h)

	var handler http.Handler = mux
	if len(authCfg.Issuers) > 0 {
		startupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		authn, err := auth.New(startupCtx, authCfg)
		if err != nil {
			return err
		}
		handler = authn.Middleware(handler)
		log.Println("auth: enforcing OIDC for issuers", authCfg.Issuers)

		// Public endpoints mounted OUTSIDE the auth middleware go on an outer mux: a
		// client must reach the RFC 9728 metadata before it has a token, and the
		// Shared Signals transmitter POSTs SETs with no bearer of ours (the SET
		// signature is the gate). The outer mux is created lazily so the no-public-
		// endpoint case keeps the bare middleware as the handler.
		var outer *http.ServeMux
		mountPublic := func(path string, h http.Handler) {
			if outer == nil {
				outer = http.NewServeMux()
				outer.Handle("/", handler)
				handler = outer
			}
			outer.Handle(path, h)
		}

		// RFC 9728: publish protected-resource metadata PUBLICLY so a client can
		// discover the authorization servers + scopes before it has a token.
		if authCfg.ResourceID != "" {
			path, metaHandler, err := auth.MetadataEndpoint(authCfg)
			if err != nil {
				return err
			}
			mountPublic(path, metaHandler)
			log.Println("auth: publishing RFC 9728 protected-resource metadata at", path)
		}

		// MB720-18: mount the Shared Signals SET receiver publicly. It verifies each
		// SET's JWS against the issuers' JWKS and feeds verified revocation events to
		// the shared store, which the auth middleware enforces on every request.
		if ssfReceiverPath != "" {
			ssfHandler, err := revocation.Handler(authCfg.Issuers, revSink)
			if err != nil {
				return err
			}
			mountPublic(ssfReceiverPath, ssfHandler)
			log.Println("revocation: Shared Signals SET receiver listening at", ssfReceiverPath)
		}
	} else {
		log.Println("auth: DISABLED (no -auth-issuer configured) — all requests allowed")
	}

	// Decompress gzip-encoded request bodies before anything reads them. The
	// official msgraph-sdk-go compresses request bodies by default, so this is
	// required for $batch and every write to interoperate with real Graph
	// clients. Outermost so it covers the auth path, public endpoints, and mux.
	handler = server.DecompressRequests(handler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Drive change-notification delivery off the inbox when the mail backend
	// supports IDLE + delta. It runs until ctx is cancelled (shutdown).
	startNotifier(ctx, provider, subStore)

	// Opt-in: turn inbound mail invitations into tentative calendar events.
	if enableScheduling {
		startScheduler(ctx, provider, calProvider)
	}

	errc := make(chan error, 1)
	go func() {
		log.Println("mailboxd listening on", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// startNotifier launches the change-notification delivery loop in a goroutine
// when the mail backend supports IMAP IDLE (mail.Watcher) and delta sync
// (mail.DeltaReader). An IDLE watch monopolizes its connection, so it dials two
// dedicated connections — one to watch, one to sync — and closes both when the
// loop stops. A missing provider or capability is a logged no-op (the
// subscriptions endpoint still accepts subscriptions; they just aren't delivered).
func startNotifier(ctx context.Context, provider server.MailProvider, store subscriptions.Store) {
	if provider == nil {
		return
	}
	watchBackend, err := provider.Mail(ctx)
	if err != nil {
		log.Println("notifications: disabled (watch connection failed):", err)
		return
	}
	watcher, ok := watchBackend.(mail.Watcher)
	if !ok {
		_ = watchBackend.Close()
		log.Println("notifications: disabled (mail backend does not support IMAP IDLE)")
		return
	}
	syncBackend, err := provider.Mail(ctx)
	if err != nil {
		_ = watchBackend.Close()
		log.Println("notifications: disabled (sync connection failed):", err)
		return
	}
	syncer, ok := syncBackend.(mail.DeltaReader)
	if !ok {
		_ = watchBackend.Close()
		_ = syncBackend.Close()
		log.Println("notifications: disabled (mail backend does not support delta)")
		return
	}

	go func() {
		defer func() { _ = watchBackend.Close(); _ = syncBackend.Close() }()
		log.Println("notifications: delivery loop watching the inbox")
		report := func(r subscriptions.Result) {
			if r.Delivered > 0 || len(r.Errors) > 0 {
				log.Printf("notifications: delivered %d/%d (errors=%d)", r.Delivered, r.Matched, len(r.Errors))
			}
		}
		if err := notify.Run(ctx, watcher, syncer, store, subscriptions.GuardedClient(), time.Now, report); err != nil {
			log.Println("notifications: delivery loop stopped:", err)
		}
	}()
}

// startScheduler launches the iTIP scheduling trigger in a goroutine when both a
// mail backend (IMAP IDLE + delta + raw) and a writable calendar backend are
// available. It watches the inbox and turns inbound REQUEST invitations into
// tentative events in the principal's first calendar; it does NOT auto-reply.
// Like the notifier it uses a dedicated watch connection plus a sync/raw one (an
// IDLE watch monopolizes its connection) and closes everything when the loop
// stops. A missing backend or capability is a logged no-op.
func startScheduler(ctx context.Context, provider server.MailProvider, calProvider server.CalendarProvider) {
	if provider == nil || calProvider == nil {
		log.Println("scheduling: disabled (needs both a mail and a calendar backend)")
		return
	}
	calBackend, err := calProvider.Calendar(ctx)
	if err != nil {
		log.Println("scheduling: disabled (calendar connection failed):", err)
		return
	}
	// RFC 6638 capability switch: if the CalDAV server schedules natively it
	// already handles inbound iTIP, so the client-side email bridge must not run —
	// it would duplicate the events the server creates. A probe failure falls
	// through to running the bridge (the safe default for a dumb server).
	if d, ok := calBackend.(calendar.SchedulingDetector); ok {
		if native, err := d.SupportsServerScheduling(ctx); err != nil {
			log.Println("scheduling: RFC 6638 probe failed, assuming client-side scheduling:", err)
		} else if native {
			_ = calBackend.Close()
			log.Println("scheduling: disabled (CalDAV server schedules natively via RFC 6638)")
			return
		}
	}
	writer, ok := calBackend.(calendar.Writer)
	if !ok {
		_ = calBackend.Close()
		log.Println("scheduling: disabled (calendar backend is read-only)")
		return
	}
	cals, err := calBackend.ListCalendars(ctx)
	if err != nil || len(cals) == 0 {
		_ = calBackend.Close()
		log.Println("scheduling: disabled (no calendar to write to):", err)
		return
	}
	calendarID := cals[0].ID

	watchBackend, err := provider.Mail(ctx)
	if err != nil {
		_ = calBackend.Close()
		log.Println("scheduling: disabled (watch connection failed):", err)
		return
	}
	watcher, ok := watchBackend.(mail.Watcher)
	if !ok {
		_ = calBackend.Close()
		_ = watchBackend.Close()
		log.Println("scheduling: disabled (mail backend does not support IMAP IDLE)")
		return
	}
	syncBackend, err := provider.Mail(ctx)
	if err != nil {
		_ = calBackend.Close()
		_ = watchBackend.Close()
		log.Println("scheduling: disabled (sync connection failed):", err)
		return
	}
	syncer, okDelta := syncBackend.(mail.DeltaReader)
	rawer, okRaw := syncBackend.(mail.RawReader)
	if !okDelta || !okRaw {
		_ = calBackend.Close()
		_ = watchBackend.Close()
		_ = syncBackend.Close()
		log.Println("scheduling: disabled (mail backend does not support delta + raw)")
		return
	}

	go func() {
		defer func() { _ = watchBackend.Close(); _ = syncBackend.Close(); _ = calBackend.Close() }()
		log.Println("scheduling: trigger watching the inbox (invitations -> tentative events; no auto-reply)")
		report := func(e calendar.Event, err error) {
			if err != nil {
				log.Println("scheduling:", err)
				return
			}
			log.Printf("scheduling: tentative event created for %q", e.Subject)
		}
		if err := schedrun.Run(ctx, watcher, syncer, rawer, writer, calendarID, report); err != nil {
			log.Println("scheduling: trigger stopped:", err)
		}
	}()
}

// buildExchanger constructs the RFC 8693 token-exchange helper per-identity
// backend providers use to mint backend-audience tokens from the authenticated
// user's token (MB720-41). It returns (nil, nil) when no endpoint is configured —
// the single-credential default. The client secret comes from the environment,
// never a flag, so it does not appear in the process table; the chosen client-
// authentication method (go-oauth-client-authn) travels on the http.Client the
// exchange uses.
func buildExchanger(endpoint, clientID, clientAuth string) (tokenexchange.Exchanger, error) {
	if endpoint == "" {
		return nil, nil
	}
	if clientID == "" {
		return nil, fmt.Errorf("token exchange: -tokenexchange-client-id is required when -tokenexchange-endpoint is set")
	}
	secret := os.Getenv("MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET")
	if secret == "" {
		return nil, fmt.Errorf("token exchange: MAILBOXD_TOKENEXCHANGE_CLIENT_SECRET is required for %s", clientAuth)
	}
	var method clientauthn.Method
	switch clientAuth {
	case "client_secret_basic":
		method = clientauthn.ClientSecretBasic(clientID, secret)
	case "client_secret_post":
		method = clientauthn.ClientSecretPost(clientID, secret)
	default:
		return nil, fmt.Errorf("token exchange: unsupported -tokenexchange-client-auth %q (want client_secret_basic or client_secret_post)", clientAuth)
	}
	return tokenexchange.New(endpoint, clientauthn.NewClient(method, nil)), nil
}

// splitList parses a comma-separated flag value into a trimmed, non-empty slice.
func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

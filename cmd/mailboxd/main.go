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
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hstern/go-mailbox-720/internal/auth"
	"github.com/hstern/go-mailbox-720/internal/calendar"
	"github.com/hstern/go-mailbox-720/internal/calendar/caldav"
	"github.com/hstern/go-mailbox-720/internal/contacts"
	"github.com/hstern/go-mailbox-720/internal/contacts/carddav"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/mail/imap"
	"github.com/hstern/go-mailbox-720/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	issuers := flag.String("auth-issuer", "", "comma-separated trusted OIDC issuer URLs (empty disables auth)")
	audience := flag.String("auth-audience", "", "expected token audience (aud)")
	scopes := flag.String("auth-scope", "", "comma-separated required scopes")
	subjectClaim := flag.String("auth-subject-claim", "sub", "token claim mapped to the mailbox identity")
	introspectID := flag.String("auth-introspect-client-id", "", "OAuth2 client id for RFC 7662 introspection of opaque tokens (enables introspection; secret from MAILBOXD_INTROSPECT_CLIENT_SECRET)")
	imapAddr := flag.String("mail-imap-addr", "", "IMAP server address host:port for the mail backend (empty: mail operations return 501; password from MAILBOXD_IMAP_PASSWORD)")
	imapUser := flag.String("mail-imap-username", "", "IMAP username for the mail backend")
	imapTLS := flag.Bool("mail-imap-tls", true, "use implicit TLS for the IMAP connection")
	caldavURL := flag.String("cal-caldav-url", "", "CalDAV base URL for the calendar backend (empty: calendar operations return 501; password from MAILBOXD_CALDAV_PASSWORD). Use an https:// URL for TLS")
	caldavUser := flag.String("cal-caldav-username", "", "CalDAV username for the calendar backend")
	carddavURL := flag.String("contacts-carddav-url", "", "CardDAV base URL for the contacts backend (empty: contacts operations return 501; password from MAILBOXD_CARDDAV_PASSWORD). Use an https:// URL for TLS")
	carddavUser := flag.String("contacts-carddav-username", "", "CardDAV username for the contacts backend")
	flag.Parse()

	cfg := auth.Config{
		Issuers:        splitList(*issuers),
		Audience:       *audience,
		RequiredScopes: splitList(*scopes),
		SubjectClaim:   *subjectClaim,
	}
	if *introspectID != "" {
		// The secret is taken from the environment, never a flag, so it does not
		// appear in the process table.
		cfg.Introspection = &auth.IntrospectionConfig{
			ClientID:     *introspectID,
			ClientSecret: os.Getenv("MAILBOXD_INTROSPECT_CLIENT_SECRET"),
		}
	}
	var provider server.MailProvider
	if *imapAddr != "" {
		provider = staticIMAPProvider{
			addr:     *imapAddr,
			username: *imapUser,
			password: os.Getenv("MAILBOXD_IMAP_PASSWORD"),
			tls:      *imapTLS,
		}
	}
	var calProvider server.CalendarProvider
	if *caldavURL != "" {
		calProvider = staticCalDAVProvider{
			url:      *caldavURL,
			username: *caldavUser,
			password: os.Getenv("MAILBOXD_CALDAV_PASSWORD"),
		}
	}
	var contactsProvider server.ContactsProvider
	if *carddavURL != "" {
		contactsProvider = staticCardDAVProvider{
			url:      *carddavURL,
			username: *carddavUser,
			password: os.Getenv("MAILBOXD_CARDDAV_PASSWORD"),
		}
	}
	if err := run(*addr, cfg, provider, calProvider, contactsProvider); err != nil {
		log.Fatalln("mailboxd:", err)
	}
}

// staticIMAPProvider serves every request from one configured IMAP account. A
// per-identity provider (mapping the token's mailbox identity to credentials,
// e.g. via Dovecot master users) is future work.
type staticIMAPProvider struct {
	addr, username, password string
	tls                      bool
}

func (p staticIMAPProvider) Mail(_ context.Context) (mail.Backend, error) {
	return imap.Dial(p.addr, p.username, p.password, &imap.Options{TLS: p.tls})
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

// staticCardDAVProvider serves every request from one configured CardDAV
// account. A per-identity provider (mapping the token's mailbox identity to
// credentials) is future work, mirroring staticIMAPProvider.
type staticCardDAVProvider struct {
	url, username, password string
}

func (p staticCardDAVProvider) Contacts(_ context.Context) (contacts.Backend, error) {
	return carddav.Dial(p.url, p.username, p.password, nil)
}

func run(addr string, authCfg auth.Config, provider server.MailProvider, calProvider server.CalendarProvider, contactsProvider server.ContactsProvider) error {
	h, err := server.New(provider, calProvider, contactsProvider)
	if err != nil {
		return err
	}
	if provider != nil {
		log.Println("mail: IMAP backend enabled")
	} else {
		log.Println("mail: no backend configured — mail operations return 501")
	}
	if calProvider != nil {
		log.Println("calendar: CalDAV backend enabled")
	} else {
		log.Println("calendar: no backend configured — calendar operations return 501")
	}
	if contactsProvider != nil {
		log.Println("contacts: CardDAV backend enabled")
	} else {
		log.Println("contacts: no backend configured — contacts operations return 501")
	}

	var handler http.Handler = h
	if len(authCfg.Issuers) > 0 {
		startupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		authn, err := auth.New(startupCtx, authCfg)
		if err != nil {
			return err
		}
		handler = authn.Middleware(handler)
		log.Println("auth: enforcing OIDC for issuers", authCfg.Issuers)
	} else {
		log.Println("auth: DISABLED (no -auth-issuer configured) — all requests allowed")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

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
	"github.com/hstern/go-mailbox-720/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	issuers := flag.String("auth-issuer", "", "comma-separated trusted OIDC issuer URLs (empty disables auth)")
	audience := flag.String("auth-audience", "", "expected token audience (aud)")
	scopes := flag.String("auth-scope", "", "comma-separated required scopes")
	subjectClaim := flag.String("auth-subject-claim", "sub", "token claim mapped to the mailbox identity")
	flag.Parse()

	cfg := auth.Config{
		Issuers:        splitList(*issuers),
		Audience:       *audience,
		RequiredScopes: splitList(*scopes),
		SubjectClaim:   *subjectClaim,
	}
	if err := run(*addr, cfg); err != nil {
		log.Fatalln("mailboxd:", err)
	}
}

func run(addr string, authCfg auth.Config) error {
	h, err := server.New()
	if err != nil {
		return err
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

// Command mailboxd runs the Microsoft Graph mailbox server.
//
// It serves the generated Graph mailbox API under /v1.0. Every operation is
// currently "not implemented" (the server skeleton, MB720-3); operations are
// filled in by later issues.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hstern/go-mailbox-720/internal/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	if err := run(*addr); err != nil {
		log.Fatalln("mailboxd:", err)
	}
}

func run(addr string) error {
	h, err := server.New()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
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

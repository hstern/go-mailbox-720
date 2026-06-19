// Package conformance black-box tests the mailbox server with Microsoft's own
// official, Kiota-generated msgraph-sdk-go client — the strongest fidelity check
// we can make: if the vendor's own SDK is happy talking to our server, the Graph
// wire contract (base-URL override, routing under /v1.0, the @odata error shape)
// holds.
//
// It lives in a separate module so msgraph-sdk-go's large dependency graph never
// touches the library's go.mod, and it drives the real mailboxd binary over HTTP
// rather than importing internal packages. The binary is taken from MAILBOXD_BIN
// or built from the parent module (which therefore must have run
// `go generate ./internal/graph` first). Auth is left disabled, matching the
// SDK's AnonymousAuthenticationProvider.
package conformance

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/microsoft/kiota-abstractions-go/authentication"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
)

// startMailboxd starts the server with auth disabled and returns its /v1.0 base
// URL. The process is torn down at test end.
func startMailboxd(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("MAILBOXD_BIN")
	if bin == "" {
		bin = buildMailboxd(t)
	}

	addr := freeAddr(t)
	cmd := exec.Command(bin, "-addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mailboxd: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	waitReady(t, "http://"+addr+"/v1.0/me/messages")
	return "http://" + addr + "/v1.0"
}

// buildMailboxd builds cmd/mailboxd from the parent module into a temp file.
func buildMailboxd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mailboxd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/mailboxd")
	cmd.Dir = "../.." // parent module root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build mailboxd (did you run `go generate ./internal/graph`?): %v", err)
	}
	return bin
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("mailboxd did not become ready in time")
}

func newClient(t *testing.T, base string) *msgraphsdk.GraphServiceClient {
	t.Helper()
	// The low-level adapter path: no credentials, no host allowlist (which would
	// reject localhost). SetBaseUrl MUST precede NewGraphServiceClient.
	adapter, err := msgraphsdk.NewGraphRequestAdapter(&authentication.AnonymousAuthenticationProvider{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.SetBaseUrl(base)
	return msgraphsdk.NewGraphServiceClient(adapter)
}

// TestMeMessagesRoundTrips drives GET /me/messages with the official SDK and
// asserts the server's not-implemented response round-trips back as a typed
// Graph ODataError (status 501, code "notImplemented") rather than a transport
// failure — proving the SDK can reach and parse our server.
func TestMeMessagesRoundTrips(t *testing.T) {
	client := newClient(t, startMailboxd(t))

	_, err := client.Me().Messages().Get(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error from the not-implemented operation")
	}

	var odErr *odataerrors.ODataError
	if !errors.As(err, &odErr) {
		t.Fatalf("error is %T, want *odataerrors.ODataError: %v", err, err)
	}
	if got := odErr.GetStatusCode(); got != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", got, http.StatusNotImplemented)
	}
	main := odErr.GetErrorEscaped()
	if main == nil || main.GetCode() == nil || *main.GetCode() != "notImplemented" {
		t.Errorf("Graph error code = %v, want notImplemented", main)
	}
}

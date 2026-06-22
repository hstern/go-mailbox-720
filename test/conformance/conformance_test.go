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
//
// No backend is configured, so every mailbox operation reports notImplemented
// (501). Each test drives one operation through the official SDK and asserts the
// 501 round-trips back as a typed Graph ODataError rather than a transport
// failure — proving the SDK can reach and parse our server across the slice
// (messages / mailFolders / events / calendars / contacts), the delta function,
// and the $batch endpoint. The server is built and started once for the whole
// package (see TestMain) because building mailboxd is the expensive step.
package conformance

import (
	"context"
	"errors"
	"fmt"
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
	msgraphgocore "github.com/microsoftgraph/msgraph-sdk-go-core"
	"github.com/microsoftgraph/msgraph-sdk-go/models/odataerrors"
)

// baseURL is the /v1.0 base URL of the shared mailboxd started by TestMain.
var baseURL string

// TestMain builds and starts a single mailboxd (auth disabled, no backend) for
// the whole package, then tears it down. Building the binary is the costly part,
// so it is done once rather than per test.
func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	bin := os.Getenv("MAILBOXD_BIN")
	if bin == "" {
		built, cleanup, err := buildMailboxd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "build mailboxd (did you run `go generate ./internal/graph`?): %v\n", err)
			return 1
		}
		defer cleanup()
		bin = built
	}

	addr, err := freeAddr()
	if err != nil {
		fmt.Fprintf(os.Stderr, "reserve addr: %v\n", err)
		return 1
	}
	cmd := exec.Command(bin, "-addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start mailboxd: %v\n", err)
		return 1
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	}()

	baseURL = "http://" + addr + "/v1.0"
	if err := waitReady(baseURL + "/me/messages"); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	return m.Run()
}

// buildMailboxd builds cmd/mailboxd from the parent module into a temp dir,
// returning the binary path and a cleanup func that removes the dir.
func buildMailboxd() (bin string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "mailboxd-conformance")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	bin = filepath.Join(dir, "mailboxd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/mailboxd")
	cmd.Dir = "../.." // parent module root
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, err
	}
	return bin, cleanup, nil
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func waitReady(url string) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("mailboxd did not become ready in time")
}

func newClient(t *testing.T) *msgraphsdk.GraphServiceClient {
	t.Helper()
	// The low-level adapter path: no credentials, no host allowlist (which would
	// reject localhost). SetBaseUrl MUST precede NewGraphServiceClient.
	adapter, err := msgraphsdk.NewGraphRequestAdapter(&authentication.AnonymousAuthenticationProvider{})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	adapter.SetBaseUrl(baseURL)
	return msgraphsdk.NewGraphServiceClient(adapter)
}

// assertNotImplemented asserts err is a typed Graph ODataError carrying status
// 501 and code "notImplemented" — the not-implemented response round-tripping
// through the SDK rather than failing at the transport.
func assertNotImplemented(t *testing.T, err error) {
	t.Helper()
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

// TestMeMessagesRoundTrips drives GET /me/messages with the official SDK and
// asserts the server's not-implemented response round-trips back as a typed
// Graph ODataError — proving the SDK can reach and parse our server.
func TestMeMessagesRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().Messages().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

func TestMeMailFoldersRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().MailFolders().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

func TestMeEventsRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().Events().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

func TestMeCalendarsRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().Calendars().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

func TestMeContactsRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().Contacts().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

// TestMeMessagesDeltaRoundTrips drives the GET /me/messages/delta() function. It
// is served by a custom handler (not a generated operation) because a delta page
// mixes full objects with @removed tombstones, so this also confirms the SDK's
// delta request builder reaches that custom route.
func TestMeMessagesDeltaRoundTrips(t *testing.T) {
	_, err := newClient(t).Me().Messages().Delta().Get(context.Background(), nil)
	assertNotImplemented(t, err)
}

// TestBatchRoundTrips composes a $batch (Graph JSON batching) holding one
// GET /me/messages step via the SDK's batch builder, sends it, and asserts the
// outer request succeeds and the sub-response carries status 501. This proves
// the SDK can build a $batch our server parses, executes, and answers with a
// well-formed multi-status batch response the SDK can parse back.
func TestBatchRoundTrips(t *testing.T) {
	client := newClient(t)
	adapter := client.GetAdapter()

	step, err := client.Me().Messages().ToGetRequestInformation(context.Background(), nil)
	if err != nil {
		t.Fatalf("build batch step: %v", err)
	}

	batch := msgraphgocore.NewBatchRequest(adapter)
	item, err := batch.AddBatchRequestStep(*step)
	if err != nil {
		t.Fatalf("add batch step: %v", err)
	}

	resp, err := batch.Send(context.Background(), adapter)
	if err != nil {
		t.Fatalf("send batch: %v", err)
	}

	id := item.GetId()
	if id == nil {
		t.Fatal("batch step has no id")
	}
	statuses := resp.GetStatusCodes()
	got, ok := statuses[*id]
	if !ok {
		t.Fatalf("no batch sub-response for step id %q; got %v", *id, statuses)
	}
	if got != http.StatusNotImplemented {
		t.Errorf("batch sub-response status = %d, want %d", got, http.StatusNotImplemented)
	}
}

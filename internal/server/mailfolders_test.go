package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// folderListBackend is a read-only mail.Backend returning a fixed folder set,
// for the MeGetMailFolders well-known-alias resolution tests.
type folderListBackend struct {
	folders []mail.MailFolder
	closed  bool
}

func (f *folderListBackend) ListMailFolders(context.Context) ([]mail.MailFolder, error) {
	return f.folders, nil
}

func (f *folderListBackend) ListMessages(context.Context, string, mail.Page, *odata.Filter) ([]mail.Message, error) {
	return nil, nil
}

func (f *folderListBackend) GetMessage(context.Context, string) (mail.Message, error) {
	return mail.Message{}, nil
}

func (f *folderListBackend) Close() error { f.closed = true; return nil }

type folderListProvider struct{ backend *folderListBackend }

func (p folderListProvider) Mail(context.Context) (mail.Backend, error) { return p.backend, nil }

func newFolderHandler() (Handler, *folderListBackend) {
	b := &folderListBackend{folders: []mail.MailFolder{
		{ID: "id-inbox", DisplayName: "INBOX", WellKnownName: "inbox", Total: 3, Unread: 1},
		{ID: "id-sent", DisplayName: "Sent", WellKnownName: "sentitems"},
		{ID: "id-proj", DisplayName: "Projects"},
	}}
	return Handler{mail: folderListProvider{backend: b}}, b
}

func getFolder(t *testing.T, h Handler, id string) *api.MicrosoftGraphMailFolderStatusCode {
	t.Helper()
	res, err := h.MeGetMailFolders(context.Background(), api.MeGetMailFoldersParams{MailFolderID: id})
	if err != nil {
		t.Fatalf("MeGetMailFolders(%q): %v", id, err)
	}
	ok, isOK := res.(*api.MicrosoftGraphMailFolderStatusCode)
	if !isOK {
		t.Fatalf("MeGetMailFolders(%q) res = %T, want MicrosoftGraphMailFolderStatusCode", id, res)
	}
	if ok.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", ok.StatusCode)
	}
	return ok
}

func TestMeGetMailFoldersResolvesWellKnownAlias(t *testing.T) {
	h, b := newFolderHandler()
	got := getFolder(t, h, "sentitems")
	if id := got.Response.ID.Value; id != "id-sent" {
		t.Errorf("alias sentitems resolved to ID %q, want id-sent", id)
	}
	if !b.closed {
		t.Error("backend not closed")
	}
}

func TestMeGetMailFoldersAliasCaseInsensitive(t *testing.T) {
	h, _ := newFolderHandler()
	got := getFolder(t, h, "Inbox")
	if id := got.Response.ID.Value; id != "id-inbox" {
		t.Errorf("alias Inbox resolved to ID %q, want id-inbox", id)
	}
}

func TestMeGetMailFoldersByOpaqueID(t *testing.T) {
	h, _ := newFolderHandler()
	got := getFolder(t, h, "id-proj")
	if id := got.Response.ID.Value; id != "id-proj" {
		t.Errorf("opaque id resolved to ID %q, want id-proj", id)
	}
}

func TestMeGetMailFoldersNotFound(t *testing.T) {
	h, _ := newFolderHandler()
	res, err := h.MeGetMailFolders(context.Background(), api.MeGetMailFoldersParams{MailFolderID: "nonexistent"})
	if err != nil {
		t.Fatalf("MeGetMailFolders: %v", err)
	}
	errRes, isErr := res.(*api.ErrorStatusCode)
	if !isErr {
		t.Fatalf("res = %T, want *api.ErrorStatusCode", res)
	}
	if errRes.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", errRes.StatusCode)
	}
}

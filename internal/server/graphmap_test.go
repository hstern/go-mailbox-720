package server

import (
	"reflect"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

func TestToGraphMessageKeywordFields(t *testing.T) {
	m := mail.Message{
		ID:         "id-1",
		Flagged:    true,
		IsDraft:    true,
		Categories: []string{"Banking", "Work"},
	}
	gm := toGraphMessage(m)

	if v, ok := gm.IsDraft.Get(); !ok || !v {
		t.Errorf("IsDraft = (%v, set=%v), want (true, true)", gm.IsDraft.Value, gm.IsDraft.Set)
	}
	if !gm.Flag.Set {
		t.Fatal("Flag unset, want a follow-up flag for a flagged message")
	}
	if got := gm.Flag.Value.FlagStatus.Value; got != api.MicrosoftGraphFollowupFlagStatusFlagged {
		t.Errorf("Flag.FlagStatus = %q, want %q", got, api.MicrosoftGraphFollowupFlagStatusFlagged)
	}
	got := make([]string, 0, len(gm.Categories))
	for _, c := range gm.Categories {
		got = append(got, c.Value)
	}
	if want := []string{"Banking", "Work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Categories = %v, want %v", got, want)
	}
}

func TestToGraphMessageUnflaggedOmitsFlagAndCategories(t *testing.T) {
	gm := toGraphMessage(mail.Message{ID: "id-2"})
	if gm.Flag.Set {
		t.Error("Flag set on an unflagged message, want unset")
	}
	if len(gm.Categories) != 0 {
		t.Errorf("Categories = %v, want empty", gm.Categories)
	}
	if v, ok := gm.IsDraft.Get(); !ok || v {
		t.Errorf("IsDraft = (%v, set=%v), want (false, true)", gm.IsDraft.Value, gm.IsDraft.Set)
	}
}

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	ht "github.com/ogen-go/ogen/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// Inbox rules / mail filters: Graph messageRule under
// /me/mailFolders/{id}/messageRules (MB720-19). These handlers translate a Graph
// messageRule to and from the neutral mail.MessageRule and drive the mail
// backend's optional FilterReader / FilterWriter capability. A backend that does
// not implement the capability yields 501 (notImplemented), the same posture as
// quota and the calendar write path.
//
// Graph nests messageRules under a mailFolder (conventionally the inbox), but the
// backend filter mechanism (Sieve) is mailbox-global, so the mailFolder id is
// accepted and ignored — the rules apply mailbox-wide. Only the /me/* variants are
// served; /users/{user-id}/* fall through to the unimplemented handler, the
// server-wide convention.

// MeMailFoldersListMessageRules implements GET /me/mailFolders/{id}/messageRules.
func (h Handler) MeMailFoldersListMessageRules(ctx context.Context, _ api.MeMailFoldersListMessageRulesParams) (api.MeMailFoldersListMessageRulesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	fr, ok := b.(mail.FilterReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	rules, err := fr.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list message rules: %w", err)
	}
	value := make([]api.MicrosoftGraphMessageRule, 0, len(rules))
	for _, r := range rules {
		value = append(value, toGraphMessageRule(r))
	}
	return &api.MicrosoftGraphMessageRuleCollectionResponseStatusCode{
		StatusCode: http.StatusOK,
		Response:   api.MicrosoftGraphMessageRuleCollectionResponse{Value: value},
	}, nil
}

// MeMailFoldersGetMessageRules implements GET /me/mailFolders/{id}/messageRules/{id}.
func (h Handler) MeMailFoldersGetMessageRules(ctx context.Context, params api.MeMailFoldersGetMessageRulesParams) (api.MeMailFoldersGetMessageRulesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	fr, ok := b.(mail.FilterReader)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	r, err := fr.GetRule(ctx, params.MessageRuleID)
	if err != nil {
		if errors.Is(err, mail.ErrRuleNotFound) {
			return notFound("message rule not found"), nil
		}
		return nil, fmt.Errorf("get message rule: %w", err)
	}
	return &api.MicrosoftGraphMessageRuleStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphMessageRule(r),
	}, nil
}

// MeMailFoldersCreateMessageRules implements POST /me/mailFolders/{id}/messageRules.
func (h Handler) MeMailFoldersCreateMessageRules(ctx context.Context, req *api.MicrosoftGraphMessageRule, _ api.MeMailFoldersCreateMessageRulesParams) (api.MeMailFoldersCreateMessageRulesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	fw, ok := b.(mail.FilterWriter)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	// A create starts from an empty rule and overlays the request's set fields.
	created, err := fw.CreateRule(ctx, mergeGraphRule(mail.MessageRule{}, req))
	if err != nil {
		return nil, fmt.Errorf("create message rule: %w", err)
	}
	return &api.MicrosoftGraphMessageRuleStatusCode{
		StatusCode: http.StatusCreated,
		Response:   toGraphMessageRule(created),
	}, nil
}

// MeMailFoldersUpdateMessageRules implements PATCH /me/mailFolders/{id}/messageRules/{id}.
// PATCH is a partial update: the existing rule is read and the request's set fields
// are overlaid onto it, so an omitted member leaves the stored value untouched. The
// merge therefore needs both capabilities; a writer realistically also reads, so a
// backend missing either yields 501.
func (h Handler) MeMailFoldersUpdateMessageRules(ctx context.Context, req *api.MicrosoftGraphMessageRule, params api.MeMailFoldersUpdateMessageRulesParams) (api.MeMailFoldersUpdateMessageRulesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	fw, okW := b.(mail.FilterWriter)
	fr, okR := b.(mail.FilterReader)
	if !okW || !okR {
		return nil, ht.ErrNotImplemented
	}
	existing, err := fr.GetRule(ctx, params.MessageRuleID)
	if err != nil {
		if errors.Is(err, mail.ErrRuleNotFound) {
			return notFound("message rule not found"), nil
		}
		return nil, fmt.Errorf("get message rule: %w", err)
	}
	updated, err := fw.UpdateRule(ctx, params.MessageRuleID, mergeGraphRule(existing, req))
	if err != nil {
		if errors.Is(err, mail.ErrRuleNotFound) {
			return notFound("message rule not found"), nil
		}
		return nil, fmt.Errorf("update message rule: %w", err)
	}
	return &api.MicrosoftGraphMessageRuleStatusCode{
		StatusCode: http.StatusOK,
		Response:   toGraphMessageRule(updated),
	}, nil
}

// MeMailFoldersDeleteMessageRules implements DELETE /me/mailFolders/{id}/messageRules/{id}.
func (h Handler) MeMailFoldersDeleteMessageRules(ctx context.Context, params api.MeMailFoldersDeleteMessageRulesParams) (api.MeMailFoldersDeleteMessageRulesRes, error) {
	b, err := h.backend(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = b.Close() }()

	fw, ok := b.(mail.FilterWriter)
	if !ok {
		return nil, ht.ErrNotImplemented
	}
	if err := fw.DeleteRule(ctx, params.MessageRuleID); err != nil {
		if errors.Is(err, mail.ErrRuleNotFound) {
			return notFound("message rule not found"), nil
		}
		return nil, fmt.Errorf("delete message rule: %w", err)
	}
	return &api.MeMailFoldersDeleteMessageRulesNoContent{}, nil
}

// mergeGraphRule overlays the set fields of a Graph messageRule body onto an
// existing neutral rule, implementing PATCH's partial-update semantics: an absent
// member leaves the existing value untouched, while a present conditions/actions
// object replaces that whole group (Graph treats messageRulePredicates and
// messageRuleActions as units). A create passes an empty base, so the overlay
// reduces to "take every set field". The read-only ID is never taken from the body.
func mergeGraphRule(base mail.MessageRule, g *api.MicrosoftGraphMessageRule) mail.MessageRule {
	r := base
	if v, ok := g.DisplayName.Get(); ok {
		r.DisplayName = v
	}
	if v, ok := g.Sequence.Get(); ok {
		r.Sequence = int(v)
	}
	if v, ok := g.IsEnabled.Get(); ok {
		r.Enabled = v
	}
	if c, ok := g.Conditions.Get(); ok {
		r.Conditions = graphToConditions(c)
	}
	if a, ok := g.Actions.Get(); ok {
		r.Actions = graphToActions(a)
	}
	return r
}

// toGraphMessageRule maps the neutral mail.MessageRule onto the generated Graph
// type. HasError and IsReadOnly are read-only Graph members the server does not
// model, so they are left unset.
func toGraphMessageRule(r mail.MessageRule) api.MicrosoftGraphMessageRule {
	return api.MicrosoftGraphMessageRule{
		ID:          api.NewOptString(r.ID),
		DisplayName: api.NewOptNilString(r.DisplayName),
		Sequence:    api.NewOptNilInt32(int32(r.Sequence)),
		IsEnabled:   api.NewOptNilBool(r.Enabled),
		Conditions:  api.NewOptMicrosoftGraphMessageRulePredicates(toGraphConditions(r.Conditions)),
		Actions:     api.NewOptMicrosoftGraphMessageRuleActions(toGraphActions(r.Actions)),
	}
}

func toGraphConditions(c mail.RuleConditions) api.MicrosoftGraphMessageRulePredicates {
	return api.MicrosoftGraphMessageRulePredicates{
		SubjectContains: toNilStrings(c.SubjectContains),
		BodyContains:    toNilStrings(c.BodyContains),
		SenderContains:  toNilStrings(c.SenderContains),
		FromAddresses:   toRecipients(c.FromAddresses),
		SentToAddresses: toRecipients(c.SentToAddresses),
	}
}

func graphToConditions(g api.MicrosoftGraphMessageRulePredicates) mail.RuleConditions {
	return mail.RuleConditions{
		SubjectContains: fromNilStrings(g.SubjectContains),
		BodyContains:    fromNilStrings(g.BodyContains),
		SenderContains:  fromNilStrings(g.SenderContains),
		FromAddresses:   graphToMailAddresses(g.FromAddresses),
		SentToAddresses: graphToMailAddresses(g.SentToAddresses),
	}
}

// toGraphActions maps neutral actions onto the Graph type. The action booleans are
// emitted only when true: a false action is simply "not taken", so it is left
// absent rather than written as an explicit false.
func toGraphActions(a mail.RuleActions) api.MicrosoftGraphMessageRuleActions {
	ga := api.MicrosoftGraphMessageRuleActions{
		ForwardTo:  toRecipients(a.ForwardTo),
		RedirectTo: toRecipients(a.RedirectTo),
	}
	if a.MoveToFolder != "" {
		ga.MoveToFolder = api.NewOptNilString(a.MoveToFolder)
	}
	if a.CopyToFolder != "" {
		ga.CopyToFolder = api.NewOptNilString(a.CopyToFolder)
	}
	if a.MarkAsRead {
		ga.MarkAsRead = api.NewOptNilBool(true)
	}
	if a.Delete {
		ga.Delete = api.NewOptNilBool(true)
	}
	if a.StopProcessingRules {
		ga.StopProcessingRules = api.NewOptNilBool(true)
	}
	return ga
}

func graphToActions(g api.MicrosoftGraphMessageRuleActions) mail.RuleActions {
	return mail.RuleActions{
		MoveToFolder:        g.MoveToFolder.Or(""),
		CopyToFolder:        g.CopyToFolder.Or(""),
		MarkAsRead:          g.MarkAsRead.Or(false),
		Delete:              g.Delete.Or(false),
		ForwardTo:           graphToMailAddresses(g.ForwardTo),
		RedirectTo:          graphToMailAddresses(g.RedirectTo),
		StopProcessingRules: g.StopProcessingRules.Or(false),
	}
}

// toNilStrings maps a []string onto the Graph []NilString predicate shape.
func toNilStrings(ss []string) []api.NilString {
	if len(ss) == 0 {
		return nil
	}
	out := make([]api.NilString, 0, len(ss))
	for _, s := range ss {
		out = append(out, api.NewNilString(s))
	}
	return out
}

// fromNilStrings maps a Graph []NilString predicate onto a []string, dropping null
// entries.
func fromNilStrings(ns []api.NilString) []string {
	if len(ns) == 0 {
		return nil
	}
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		if v, ok := n.Get(); ok {
			out = append(out, v)
		}
	}
	return out
}

// graphToMailAddresses maps Graph recipients onto neutral mail.Address values,
// the inverse of toRecipients. Recipients without an emailAddress are skipped.
func graphToMailAddresses(rs []api.MicrosoftGraphRecipient) []mail.Address {
	if len(rs) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(rs))
	for _, r := range rs {
		ea, ok := r.EmailAddress.Get()
		if !ok {
			continue
		}
		out = append(out, mail.Address{Name: ea.Name.Or(""), Email: ea.Address.Or("")})
	}
	return out
}

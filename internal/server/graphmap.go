package server

import (
	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/mail"
)

// toGraphMessage maps the neutral mail.Message onto the generated Graph type.
func toGraphMessage(m mail.Message) api.MicrosoftGraphMessage {
	gm := api.MicrosoftGraphMessage{
		ID:             api.NewOptString(m.ID),
		Subject:        api.NewOptNilString(m.Subject),
		BodyPreview:    api.NewOptNilString(m.Preview),
		IsRead:         api.NewOptNilBool(m.IsRead),
		IsDraft:        api.NewOptNilBool(m.IsDraft),
		HasAttachments: api.NewOptNilBool(m.HasAttachments),
		Categories:     graphCategories(m.Categories),
		ToRecipients:   toRecipients(m.To),
		CcRecipients:   toRecipients(m.Cc),
	}
	// A flagged message surfaces Graph's follow-up flag with flagStatus=flagged;
	// an unflagged one leaves flag unset rather than emitting notFlagged noise.
	if m.Flagged {
		gm.Flag = api.NewOptMicrosoftGraphFollowupFlag(api.MicrosoftGraphFollowupFlag{
			FlagStatus: api.NewOptMicrosoftGraphFollowupFlagStatus(api.MicrosoftGraphFollowupFlagStatusFlagged),
		})
	}
	if !m.ReceivedAt.IsZero() {
		gm.ReceivedDateTime = api.NewOptNilDateTime(m.ReceivedAt)
	}
	if !m.SentAt.IsZero() {
		gm.SentDateTime = api.NewOptNilDateTime(m.SentAt)
	}
	if m.From.Email != "" {
		gm.From = api.NewOptMicrosoftGraphRecipient(toRecipient(m.From))
	}
	if m.Body.Content != "" {
		gm.Body = api.NewOptMicrosoftGraphItemBody(api.MicrosoftGraphItemBody{
			Content:     api.NewOptNilString(m.Body.Content),
			ContentType: api.NewOptMicrosoftGraphBodyType(graphBodyType(m.Body.ContentType)),
		})
	}
	return gm
}

// graphCategories maps the neutral category strings onto Graph's categories
// slice ([]NilString). It returns nil for no categories so the field is omitted
// rather than serialized as an empty array.
func graphCategories(cats []string) []api.NilString {
	if len(cats) == 0 {
		return nil
	}
	out := make([]api.NilString, 0, len(cats))
	for _, c := range cats {
		out = append(out, api.NewNilString(c))
	}
	return out
}

func graphBodyType(contentType string) api.MicrosoftGraphBodyType {
	if contentType == "html" {
		return api.MicrosoftGraphBodyTypeHTML
	}
	return api.MicrosoftGraphBodyTypeText
}

func toRecipient(a mail.Address) api.MicrosoftGraphRecipient {
	return api.MicrosoftGraphRecipient{
		EmailAddress: api.NewOptMicrosoftGraphEmailAddress(api.MicrosoftGraphEmailAddress{
			Name:    api.NewOptNilString(a.Name),
			Address: api.NewOptNilString(a.Email),
		}),
	}
}

func toRecipients(as []mail.Address) []api.MicrosoftGraphRecipient {
	if len(as) == 0 {
		return nil
	}
	out := make([]api.MicrosoftGraphRecipient, 0, len(as))
	for _, a := range as {
		out = append(out, toRecipient(a))
	}
	return out
}

// toGraphFolder maps the neutral mail.MailFolder onto the generated Graph type.
func toGraphFolder(f mail.MailFolder) api.MicrosoftGraphMailFolder {
	return api.MicrosoftGraphMailFolder{
		ID:              api.NewOptString(f.ID),
		DisplayName:     api.NewOptNilString(f.DisplayName),
		TotalItemCount:  api.NewOptNilInt32(int32(f.Total)),
		UnreadItemCount: api.NewOptNilInt32(int32(f.Unread)),
	}
}

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
		HasAttachments: api.NewOptNilBool(m.HasAttachments),
		ToRecipients:   toRecipients(m.To),
		CcRecipients:   toRecipients(m.Cc),
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

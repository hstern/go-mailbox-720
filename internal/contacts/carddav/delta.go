package carddav

import (
	"context"
	"fmt"

	gocarddav "github.com/emersion/go-webdav/carddav"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

var _ contacts.DeltaReader = (*Client)(nil)

// Delta reports the contacts in an address book that changed since the opaque
// token via a CardDAV sync-collection REPORT (RFC 6578). An empty token means
// initial sync: the server returns the current contents plus a fresh sync-token.
// The returned next token is the sync-response's SyncToken, fed back on the
// following call to fetch only what changed since.
//
// It maps the sync-response's updated address objects to Contacts (stamping each
// with its opaque id, the same codec as the read paths), and returns the deleted
// hrefs as opaque IDs the caller can emit as Graph @removed tombstones.
//
// go-webdav gotcha: a sync-collection response reports only WHICH hrefs changed
// (path/etag/last-modified) — go-webdav's SyncResponse.Updated objects carry no
// vCard, even when address-data is requested in the query. RFC 6578 expects the
// client to follow up to fetch the cards. We therefore GET each updated object
// (the same call GetContact uses) and map it through contactFromObject. An
// updated object whose card cannot be fetched or mapped is skipped rather than
// failing the whole delta, mirroring ListContacts.
func (cl *Client) Delta(ctx context.Context, abID string, token string) (changed []contacts.Contact, removed []string, next string, err error) {
	abPath, err := decodeAddressBookID(abID)
	if err != nil {
		return nil, nil, "", err
	}
	query := &gocarddav.SyncQuery{
		// Ask for the vCard data; even though go-webdav does not surface it on
		// the sync response (see the doc comment), requesting it keeps the wire
		// request RFC-correct for servers that do return data inline.
		DataRequest: gocarddav.AddressDataRequest{AllProp: true},
		SyncToken:   token,
	}
	res, err := cl.c.SyncCollection(ctx, abPath, query)
	if err != nil {
		return nil, nil, "", fmt.Errorf("carddav: delta: %w", err)
	}
	for _, obj := range res.Updated {
		card := obj.Card
		if len(card) == 0 {
			// The sync response listed the href but not its card; fetch it.
			full, gerr := cl.c.GetAddressObject(ctx, obj.Path)
			if gerr != nil {
				return nil, nil, "", fmt.Errorf("carddav: delta: get %q: %w", obj.Path, gerr)
			}
			card = full.Card
		}
		if c, ok := contactFromObject(abID, obj.Path, card); ok {
			changed = append(changed, c)
		}
	}
	for _, href := range res.Deleted {
		removed = append(removed, contactID(href))
	}
	return changed, removed, res.SyncToken, nil
}

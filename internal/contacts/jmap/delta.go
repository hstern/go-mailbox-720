package jmap

import (
	"context"
	"fmt"

	gojmap "git.sr.ht/~rockorager/go-jmap"

	"github.com/hstern/go-mailbox-720/internal/contacts"
)

// Verify that Client implements the DeltaReader interface.
var _ contacts.DeltaReader = (*Client)(nil)

// Delta returns the contacts in addressBookID that have changed since the opaque
// token (a JMAP state string). It issues a ContactCard/changes call and then
// fetches the created+updated cards via ContactCard/get.
//
// Address-book filtering for changed cards: ContactCard/changes is account-wide,
// not scoped to a single address book. We fetch all created/updated cards and
// retain only those whose addressBookIds include the requested addressBookID;
// cards in other address books are silently dropped. This mirrors the JMAP
// calendar adapter's per-calendar filtering.
//
// Destroyed cards cannot be fetched (they no longer exist on the server), so we
// cannot determine which address book they belonged to. All destroyed IDs are
// included in removed regardless of address book — callers should treat removed
// as a superset and handle tombstones idempotently.
//
// Limitation: a single call returns one page of changes. If the server sets
// hasMoreChanges, additional changes exist beyond the returned next token; the
// caller must call Delta again with next until the server stops advancing. This
// method does not loop internally.
func (cl *Client) Delta(ctx context.Context, addressBookID string, token string) (changed []contacts.Contact, removed []string, next string, err error) {
	cArgs, err := cl.do(ctx, &cardChanges{
		Account:    cl.accountID,
		SinceState: token,
	})
	if err != nil {
		return nil, nil, "", fmt.Errorf("jmap: ContactCard/changes: %w", err)
	}
	cResp, ok := cArgs.(*cardChangesResponse)
	if !ok {
		return nil, nil, "", fmt.Errorf("jmap: unexpected response %T for ContactCard/changes", cArgs)
	}

	next = cResp.NewState

	if len(cResp.Destroyed) > 0 {
		removed = make([]string, len(cResp.Destroyed))
		for i, id := range cResp.Destroyed {
			removed[i] = string(id)
		}
	}

	toFetch := make([]gojmap.ID, 0, len(cResp.Created)+len(cResp.Updated))
	toFetch = append(toFetch, cResp.Created...)
	toFetch = append(toFetch, cResp.Updated...)
	if len(toFetch) == 0 {
		return nil, removed, next, nil
	}

	gargs, err := cl.do(ctx, &cardGet{Account: cl.accountID, IDs: toFetch})
	if err != nil {
		return nil, nil, "", fmt.Errorf("jmap: ContactCard/get: %w", err)
	}
	gresp, ok := gargs.(*cardGetResponse)
	if !ok {
		return nil, nil, "", fmt.Errorf("jmap: unexpected response %T for ContactCard/get", gargs)
	}

	for _, cc := range gresp.List {
		if cc.AddressBookIDs[addressBookID] {
			changed = append(changed, contactFromCard(cc, addressBookID))
		}
	}

	return changed, removed, next, nil
}

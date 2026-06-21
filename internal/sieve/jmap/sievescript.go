// Package jmap implements the Sieve-script management transport over JMAP for
// Sieve Scripts (RFC 9661). go-jmap (git.sr.ht/~rockorager/go-jmap) provides the
// protocol core but no Sieve methods, so this package defines the SieveScript/get,
// SieveScript/set, and SieveScript/validate calls and a small client over them.
//
// The transport treats a script's content as opaque UTF-8 Sieve text: it uploads
// and downloads the bytes and manages the SieveScript objects (name, blobId,
// active flag), but does not parse, generate, or validate Sieve locally — the
// server's SieveScript/validate does that remotely. Translating go-mailbox-720's
// neutral mail.MessageRule to and from Sieve text — the consumer-side step that
// turns this transport into a mail.FilterReader/FilterWriter — is layered on top
// once go-sieve (RFC 5228) lands (MB720-19 chunk C). This is the JMAP-tier sibling
// of the ManageSieve (RFC 5804) transport.
package jmap

import (
	gojmap "git.sr.ht/~rockorager/go-jmap"
)

// sieveURI is the RFC 9661 JMAP capability URN for Sieve scripts.
const sieveURI gojmap.URI = "urn:ietf:params:jmap:sieve"

func init() {
	gojmap.RegisterMethod("SieveScript/get", func() gojmap.MethodResponse { return &sieveGetResponse{} })
	gojmap.RegisterMethod("SieveScript/set", func() gojmap.MethodResponse { return &sieveSetResponse{} })
	gojmap.RegisterMethod("SieveScript/validate", func() gojmap.MethodResponse { return &sieveValidateResponse{} })
}

// sieveScript is the RFC 9661 §2 SieveScript object. id and isActive are server-set;
// name is unique within the account; blobId references the raw script octets.
type sieveScript struct {
	ID       gojmap.ID `json:"id,omitempty"`
	Name     string    `json:"name,omitempty"`
	BlobID   gojmap.ID `json:"blobId,omitempty"`
	IsActive bool      `json:"isActive,omitempty"`
}

// --- SieveScript/get (RFC 9661 §2.1) ---

type sieveGet struct {
	Account gojmap.ID `json:"accountId,omitempty"`
	// IDs is the scripts to fetch. A nil slice marshals to JSON null, which RFC 9661
	// defines as "fetch all scripts" — the absence of omitempty is deliberate.
	IDs []gojmap.ID `json:"ids"`
}

func (m *sieveGet) Name() string           { return "SieveScript/get" }
func (m *sieveGet) Requires() []gojmap.URI { return []gojmap.URI{sieveURI} }

type sieveGetResponse struct {
	Account  gojmap.ID      `json:"accountId,omitempty"`
	State    string         `json:"state,omitempty"`
	List     []*sieveScript `json:"list,omitempty"`
	NotFound []gojmap.ID    `json:"notFound,omitempty"`
}

// --- SieveScript/set (RFC 9661 §2.4) ---

type sieveSet struct {
	Account gojmap.ID                     `json:"accountId,omitempty"`
	Create  map[string]*sieveScriptCreate `json:"create,omitempty"`
	Update  map[gojmap.ID]map[string]any  `json:"update,omitempty"`
	Destroy []gojmap.ID                   `json:"destroy,omitempty"`
	// ActivateScript names a script (or a "#creationId" reference) to make active
	// once the create/update/destroy succeed; DeactivateScript clears the active
	// script first. RFC 9661 processes deactivate before activate.
	ActivateScript   *gojmap.ID `json:"onSuccessActivateScript,omitempty"`
	DeactivateScript bool       `json:"onSuccessDeactivateScript,omitempty"`
}

// sieveScriptCreate is a create-side SieveScript: only the writable members. blobId
// carries no omitempty — a create without a script blob is invalid and should be
// sent (and rejected) explicitly rather than silently dropped.
type sieveScriptCreate struct {
	Name   string    `json:"name,omitempty"`
	BlobID gojmap.ID `json:"blobId"`
}

func (m *sieveSet) Name() string           { return "SieveScript/set" }
func (m *sieveSet) Requires() []gojmap.URI { return []gojmap.URI{sieveURI} }

type sieveSetResponse struct {
	Account      gojmap.ID                      `json:"accountId,omitempty"`
	NewState     string                         `json:"newState,omitempty"`
	Created      map[string]*sieveScript        `json:"created,omitempty"`
	Updated      map[gojmap.ID]*sieveScript     `json:"updated,omitempty"`
	Destroyed    []gojmap.ID                    `json:"destroyed,omitempty"`
	NotCreated   map[string]*gojmap.SetError    `json:"notCreated,omitempty"`
	NotUpdated   map[gojmap.ID]*gojmap.SetError `json:"notUpdated,omitempty"`
	NotDestroyed map[gojmap.ID]*gojmap.SetError `json:"notDestroyed,omitempty"`
}

// --- SieveScript/validate (RFC 9661 §2.6) ---

type sieveValidate struct {
	Account gojmap.ID `json:"accountId,omitempty"`
	BlobID  gojmap.ID `json:"blobId"`
}

func (m *sieveValidate) Name() string           { return "SieveScript/validate" }
func (m *sieveValidate) Requires() []gojmap.URI { return []gojmap.URI{sieveURI} }

type sieveValidateResponse struct {
	Account gojmap.ID `json:"accountId,omitempty"`
	// Error is null when the script is valid, else an "invalidSieve" SetError whose
	// description explains the problem.
	Error *gojmap.SetError `json:"error,omitempty"`
}

package server

import (
	"net/http"

	"github.com/hstern/go-mailbox-720/internal/graph/api"
	"github.com/hstern/go-mailbox-720/internal/odata"
)

// parseMessageFilter turns the optional $filter query option into a parsed,
// validated odata.Filter. It returns (nil, nil) when no filter was supplied (the
// caller then lists without filtering). On a malformed or unsupported filter it
// returns (nil, errRes) where errRes is a Graph 400 response ready to return; on
// success it returns (filter, nil).
func parseMessageFilter(raw api.OptString) (*odata.Filter, *api.ErrorStatusCode) {
	value, ok := raw.Get()
	if !ok || value == "" {
		return nil, nil
	}
	filter, err := odata.Parse(value)
	if err != nil {
		return nil, badFilterResponse(err)
	}
	if err := filter.Validate(messageFilterFields); err != nil {
		return nil, badFilterResponse(err)
	}
	return filter, nil
}

// badFilterResponse builds the Graph 400 response for an invalid $filter. It
// carries the parse/validation message through verbatim so a client sees why the
// filter was rejected (the odata sentinels — malformed, unsupported operator/
// function, unknown field — all map to the same 400).
func badFilterResponse(err error) *api.ErrorStatusCode {
	return badRequest(err.Error())
}

// badRequest builds a Graph 400 (BadRequest) error response carrying message.
// *ErrorStatusCode satisfies every operation's response interface, so handlers
// return it directly to reject malformed input.
func badRequest(message string) *api.ErrorStatusCode {
	return &api.ErrorStatusCode{
		StatusCode: http.StatusBadRequest,
		Response: api.MicrosoftGraphODataErrorsODataError{
			Error: api.MicrosoftGraphODataErrorsMainError{
				Code:    "BadRequest",
				Message: message,
			},
		},
	}
}

// resyncRequired builds the Graph 410 (Gone) a delta sync returns when the
// client's continuation token is no longer valid: the client drops the token and
// restarts with an initial sync. Delta libraries key their recovery off this.
func resyncRequired() *api.ErrorStatusCode {
	return &api.ErrorStatusCode{
		StatusCode: http.StatusGone,
		Response: api.MicrosoftGraphODataErrorsODataError{
			Error: api.MicrosoftGraphODataErrorsMainError{
				Code:    "resyncRequired",
				Message: "The delta token is invalid or expired; restart with an initial sync.",
			},
		},
	}
}

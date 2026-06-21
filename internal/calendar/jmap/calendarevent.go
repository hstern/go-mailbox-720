package jmap

import (
	gojmap "git.sr.ht/~rockorager/go-jmap"
	jscal "github.com/hstern/go-jscalendar/jmap"
)

func init() {
	gojmap.RegisterMethod("Calendar/get", func() gojmap.MethodResponse { return &calendarGetResponse{} })
	gojmap.RegisterMethod("CalendarEvent/get", func() gojmap.MethodResponse { return &eventGetResponse{} })
	gojmap.RegisterMethod("CalendarEvent/query", func() gojmap.MethodResponse { return &eventQueryResponse{} })
	gojmap.RegisterMethod("CalendarEvent/changes", func() gojmap.MethodResponse { return &eventChangesResponse{} })
	gojmap.RegisterMethod("CalendarEvent/set", func() gojmap.MethodResponse { return &eventSetResponse{} })
}

// --- Calendar/get ---

type jmapCalendar struct {
	ID          gojmap.ID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

type calendarGet struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids,omitempty"`
}

func (m *calendarGet) Name() string             { return "Calendar/get" }
func (m *calendarGet) Requires() []gojmap.URI   { return []gojmap.URI{calendarsURI} }

type calendarGetResponse struct {
	Account gojmap.ID       `json:"accountId"`
	State   string          `json:"state"`
	List    []*jmapCalendar `json:"list"`
}

// --- CalendarEvent/get ---

type eventGet struct {
	Account    gojmap.ID   `json:"accountId"`
	IDs        []gojmap.ID `json:"ids,omitempty"`
	Properties []string    `json:"properties,omitempty"`
}

func (m *eventGet) Name() string           { return "CalendarEvent/get" }
func (m *eventGet) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventGetResponse struct {
	Account  gojmap.ID             `json:"accountId"`
	State    string                `json:"state"`
	List     []*jscal.CalendarEvent `json:"list"`
	NotFound []gojmap.ID           `json:"notFound"`
}

// --- CalendarEvent/query ---

type eventFilter struct {
	InCalendars []gojmap.ID `json:"inCalendars,omitempty"`
	After       string      `json:"after,omitempty"`
	Before      string      `json:"before,omitempty"`
	UID         string      `json:"uid,omitempty"`
}

type eventQuery struct {
	Account           gojmap.ID    `json:"accountId"`
	Filter            *eventFilter `json:"filter,omitempty"`
	ExpandRecurrences bool         `json:"expandRecurrences,omitempty"`
}

func (m *eventQuery) Name() string           { return "CalendarEvent/query" }
func (m *eventQuery) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventQueryResponse struct {
	Account gojmap.ID   `json:"accountId"`
	IDs     []gojmap.ID `json:"ids"`
}

// --- CalendarEvent/changes ---

type eventChanges struct {
	Account    gojmap.ID `json:"accountId"`
	SinceState string    `json:"sinceState"`
	MaxChanges uint64    `json:"maxChanges,omitempty"`
}

func (m *eventChanges) Name() string           { return "CalendarEvent/changes" }
func (m *eventChanges) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventChangesResponse struct {
	Account        gojmap.ID   `json:"accountId"`
	OldState       string      `json:"oldState"`
	NewState       string      `json:"newState"`
	HasMoreChanges bool        `json:"hasMoreChanges"`
	Created        []gojmap.ID `json:"created"`
	Updated        []gojmap.ID `json:"updated"`
	Destroyed      []gojmap.ID `json:"destroyed"`
}

// --- CalendarEvent/set ---

type eventSet struct {
	Account                gojmap.ID                        `json:"accountId"`
	Create                 map[gojmap.ID]*jscal.CalendarEvent `json:"create,omitempty"`
	Update                 map[gojmap.ID]gojmap.Patch        `json:"update,omitempty"`
	Destroy                []gojmap.ID                       `json:"destroy,omitempty"`
	SendSchedulingMessages bool                              `json:"sendSchedulingMessages,omitempty"`
}

func (m *eventSet) Name() string           { return "CalendarEvent/set" }
func (m *eventSet) Requires() []gojmap.URI { return []gojmap.URI{calendarsURI} }

type eventSetResponse struct {
	Account      gojmap.ID                          `json:"accountId"`
	OldState     string                             `json:"oldState"`
	NewState     string                             `json:"newState"`
	Created      map[gojmap.ID]*jscal.CalendarEvent `json:"created"`
	Updated      map[gojmap.ID]*jscal.CalendarEvent `json:"updated"`
	Destroyed    []gojmap.ID                        `json:"destroyed"`
	NotCreated   map[gojmap.ID]*gojmap.SetError     `json:"notCreated"`
	NotUpdated   map[gojmap.ID]*gojmap.SetError     `json:"notUpdated"`
	NotDestroyed map[gojmap.ID]*gojmap.SetError     `json:"notDestroyed"`
}

//go:build dockertest

// Integration test for the JMAP calendar-sharing adapter (PermissionReader/Writer)
// against a real Stalwart server. Shares the user's own calendar with their own
// principal (Stalwart accepts a self-grant), which exercises the full RFC 9670 +
// shareWith round-trip without provisioning a second account. Run with:
//
//	go test -tags dockertest ./internal/calendar/jmap/ -run TestStalwartCalendarSharing

package jmap

import (
	"context"
	"os/exec"
	"testing"

	"github.com/hstern/go-mailbox-720/internal/calendar"
)

func TestStalwartCalendarSharing(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	sessionURL, login, password, apiURL, _ := jmapStartStalwart(t)
	ctx := context.Background()

	cl, err := Dial(sessionURL, "", &Options{
		BasicAuth:      &BasicAuthCredentials{Username: login, Password: password},
		APIURLOverride: apiURL,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cl.Close() }()

	cals, err := cl.ListCalendars(ctx)
	if err != nil || len(cals) == 0 {
		t.Fatalf("ListCalendars = %v, %v", cals, err)
	}
	calID := cals[0].ID

	// Grant read access (to the user's own principal — a self-share Stalwart allows).
	created, err := cl.CreateCalendarPermission(ctx, calID, calendar.Permission{Email: login, Role: calendar.RoleRead})
	if err != nil {
		t.Fatalf("CreateCalendarPermission: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created permission has no principal id")
	}

	perms, err := cl.ListCalendarPermissions(ctx, calID)
	if err != nil {
		t.Fatalf("ListCalendarPermissions: %v", err)
	}
	var found *calendar.Permission
	for i := range perms {
		if perms[i].ID == created.ID {
			found = &perms[i]
		}
	}
	if found == nil {
		t.Fatalf("granted permission not listed: %+v", perms)
	}
	if found.Email != login || found.Role != calendar.RoleRead {
		t.Errorf("listed permission = %+v, want email=%s role=read", *found, login)
	}

	// Promote to write, then revoke.
	if _, err := cl.UpdateCalendarPermission(ctx, calID, created.ID, calendar.Permission{Role: calendar.RoleWrite}); err != nil {
		t.Fatalf("UpdateCalendarPermission: %v", err)
	}
	if got, err := cl.GetCalendarPermission(ctx, calID, created.ID); err != nil || got.Role != calendar.RoleWrite {
		t.Errorf("after update: %+v, %v; want role=write", got, err)
	}
	if err := cl.DeleteCalendarPermission(ctx, calID, created.ID); err != nil {
		t.Fatalf("DeleteCalendarPermission: %v", err)
	}
	if perms, _ := cl.ListCalendarPermissions(ctx, calID); len(perms) != 0 {
		t.Errorf("after delete, still shared: %+v", perms)
	}
}

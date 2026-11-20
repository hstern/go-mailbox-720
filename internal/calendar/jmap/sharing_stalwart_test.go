//go:build dockertest

// Integration test for the JMAP calendar-sharing adapter (PermissionReader/Writer)
// against a real Stalwart server. Shares the user's calendar with a second
// provisioned account (Stalwart v1.x rejects a self-grant), which exercises the full RFC 9670 +
// shareWith round-trip. Run with:
//
//	go test -tags dockertest ./internal/calendar/jmap/ -run TestStalwartCalendarSharing

package jmap

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	gojmap "git.sr.ht/~rockorager/go-jmap"

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

	sharee := jmapStalwartShareeUser + "@" + jmapStalwartDomain

	// Grant read access to the second account.
	created, err := cl.CreateCalendarPermission(ctx, calID, calendar.Permission{Email: sharee, Role: calendar.RoleRead})
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
	if found.Email != sharee || found.Role != calendar.RoleRead {
		t.Errorf("listed permission = %+v, want email=%s role=read", *found, sharee)
	}

	if len(created.AllowedRoles) == 0 || len(found.AllowedRoles) == 0 {
		t.Errorf("AllowedRoles not populated: created=%v listed=%v", created.AllowedRoles, found.AllowedRoles)
	}

	// Roles the JMAP tier cannot enforce are refused, not downgraded.
	for _, role := range []calendar.PermissionRole{calendar.RoleDelegatePrivate, calendar.RoleLimitedRead} {
		if _, err := cl.UpdateCalendarPermission(ctx, calID, created.ID, calendar.Permission{Role: role}); !errors.Is(err, calendar.ErrRoleNotAllowed) {
			t.Errorf("update to %q: err = %v, want ErrRoleNotAllowed", role, err)
		}
	}

	// Promote to write: it must read back as write without mayDelete (which in
	// JMAP Calendars deletes the calendar itself).
	if _, err := cl.UpdateCalendarPermission(ctx, calID, created.ID, calendar.Permission{Role: calendar.RoleWrite}); err != nil {
		t.Fatalf("UpdateCalendarPermission: %v", err)
	}
	if got, err := cl.GetCalendarPermission(ctx, calID, created.ID); err != nil || got.Role != calendar.RoleWrite {
		t.Errorf("after update: %+v, %v; want role=write", got, err)
	}
	cal, err := cl.getShared(ctx, calID)
	if err != nil {
		t.Fatalf("getShared: %v", err)
	}
	if r := cal.ShareWith[gojmap.ID(created.ID)]; r == nil || !r.MayWriteAll || r.MayDelete || r.MayShare {
		t.Errorf("stored write rights = %+v, want mayWriteAll without mayDelete/mayShare", r)
	}
	if err := cl.DeleteCalendarPermission(ctx, calID, created.ID); err != nil {
		t.Fatalf("DeleteCalendarPermission: %v", err)
	}
	if perms, _ := cl.ListCalendarPermissions(ctx, calID); len(perms) != 0 {
		t.Errorf("after delete, still shared: %+v", perms)
	}
}

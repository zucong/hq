package main

import (
	"context"
	"strings"
	"testing"
)

func TestRefreshMaintenanceBindingFollowsAuthorizedSeatIncarnation(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	rule, _ := app.Config.exactRule("penny")
	rule.CanManageStaff = true
	for index := range app.Config.Agents {
		if app.Config.Agents[index].Name == rule.Name {
			app.Config.Agents[index].CanManageStaff = true
			app.Config.Agents[index].SeatDigest = employeeSeatDigest(app.Config.Agents[index])
		}
	}
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, rule, "idle")
	control.snapshot.Panes[0].ID = "w-test:p-new"
	control.snapshot.Agents[0].PaneID = "w-test:p-new"
	app.Herdr = control
	app.MaintenanceActor = rule.Name
	app.MaintenancePane = "w-test:p-retired"
	app.GatewayWorkspaceID = "w-test"

	if err := app.refreshMaintenanceBinding(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.MaintenancePane != "w-test:p-new" {
		t.Fatalf("maintenance pane did not follow the same seat: %q", app.MaintenancePane)
	}
}

func TestRefreshMaintenanceBindingNeverFallsBackToStalePane(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	for index := range app.Config.Agents {
		if app.Config.Agents[index].Name == "penny" {
			app.Config.Agents[index].CanManageStaff = true
			app.Config.Agents[index].SeatDigest = employeeSeatDigest(app.Config.Agents[index])
		}
	}
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	control.snapshot.Agents = nil
	app.Herdr = control
	app.MaintenanceActor = "penny"
	app.MaintenancePane = "w-test:p-retired"
	app.GatewayWorkspaceID = "w-test"

	err := app.refreshMaintenanceBinding(context.Background())
	if err == nil || !strings.Contains(err.Error(), "暂停内部 nudge") {
		t.Fatalf("missing corrective stale-binding error: %v", err)
	}
	// The caller clears this value before running watchdogs. The refresh
	// method itself never substitutes another seat or workspace.
	if app.MaintenancePane != "w-test:p-retired" {
		t.Fatalf("failed refresh unexpectedly rewrote pane: %q", app.MaintenancePane)
	}
}

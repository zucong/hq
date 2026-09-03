package main

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func workspaceScopeSnapshot(root string) HerdrSnapshot {
	targetCWD := filepath.Join(root, "engineering")
	return HerdrSnapshot{
		Workspaces: []HerdrWorkspace{{ID: "w-target", Label: "hq-test"}, {ID: "w-other", Label: "other"}},
		Tabs: []HerdrTab{
			// Protocol 20 does not expose cwd on tab objects. The decoder must
			// derive it from complete, consistent pane evidence.
			{ID: "w-target:t1", WorkspaceID: "w-target", Label: "工程部-李春"},
			{ID: "w-other:t1", WorkspaceID: "w-other", Label: "legacy", CWD: ""},
		},
		Panes: []HerdrPane{
			{ID: "w-target:p1", WorkspaceID: "w-target", TabID: "w-target:t1", CWD: targetCWD},
			{ID: "w-other:p1", WorkspaceID: "w-other", TabID: "w-other:t1", CWD: ""},
		},
		Agents: []HerdrAgent{
			{Name: "lichun", Kind: "codex", Status: "working", CWD: targetCWD, WorkspaceID: "w-target", TabID: "w-target:t1", PaneID: "w-target:p1", InteractiveReady: true},
			{Name: "other-agent", Kind: "codex", Status: "idle", CWD: "", WorkspaceID: "w-other", TabID: "w-other:t1", PaneID: "w-other:p1"},
		},
	}
}

func decodeWorkspaceScopeSnapshot(t *testing.T, snapshot HerdrSnapshot) error {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"result": map[string]any{"snapshot": snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeHerdrSnapshot(raw, HerdrSnapshotScope{WorkspaceLabel: "hq-test"})
	return err
}

func TestWorkspaceScopeWorkspaceScopedSnapshotSecurityMatrix(t *testing.T) {
	root := canonicalTestTempDir(t)
	base := workspaceScopeSnapshot(root)
	if err := decodeWorkspaceScopeSnapshot(t, base); err != nil {
		t.Fatalf("unrelated known workspace missing cwd must be isolated: %v", err)
	}
	unmanaged := cloneHerdrSnapshot(base)
	unmanaged.Agents[1].Name, unmanaged.Agents[1].Kind, unmanaged.Agents[1].Status = "", "", ""
	if err := decodeWorkspaceScopeSnapshot(t, unmanaged); err != nil {
		t.Fatalf("unrelated structurally-owned unmanaged agent must be isolated: %v", err)
	}

	tests := []struct {
		name string
		want string
		edit func(*HerdrSnapshot)
	}{
		{name: "target tab missing label", want: "缺少", edit: func(s *HerdrSnapshot) { s.Tabs[0].Label = "" }},
		{name: "target pane missing cwd", want: "缺少", edit: func(s *HerdrSnapshot) { s.Panes[0].CWD = "" }},
		{name: "target agent missing cwd", want: "缺少", edit: func(s *HerdrSnapshot) { s.Agents[0].CWD = "" }},
		{name: "target agent missing name", want: "缺少", edit: func(s *HerdrSnapshot) { s.Agents[0].Name = "" }},
		{name: "target pane cwd conflicts with agent", want: "cwd 冲突", edit: func(s *HerdrSnapshot) { s.Panes[0].CWD = filepath.Join(root, "wrong") }},
		{name: "tab missing workspace id", want: "缺少", edit: func(s *HerdrSnapshot) { s.Tabs[1].WorkspaceID = "" }},
		{name: "pane missing workspace id", want: "缺少", edit: func(s *HerdrSnapshot) { s.Panes[1].WorkspaceID = "" }},
		{name: "agent missing workspace id", want: "缺少", edit: func(s *HerdrSnapshot) { s.Agents[1].WorkspaceID = "" }},
		{name: "unknown workspace ownership", want: "归属不明", edit: func(s *HerdrSnapshot) { s.Tabs[1].WorkspaceID = "w-unknown" }},
		{name: "duplicate workspace stable id", want: "稳定 ID", edit: func(s *HerdrSnapshot) { s.Workspaces = append(s.Workspaces, s.Workspaces[0]) }},
		{name: "duplicate tab stable id", want: "稳定 ID", edit: func(s *HerdrSnapshot) { s.Tabs = append(s.Tabs, s.Tabs[0]) }},
		{name: "duplicate pane stable id", want: "稳定 ID", edit: func(s *HerdrSnapshot) { s.Panes = append(s.Panes, s.Panes[0]) }},
		{name: "duplicate agent stable id", want: "稳定 ID", edit: func(s *HerdrSnapshot) { s.Agents = append(s.Agents, s.Agents[0]) }},
		{name: "tab fingerprint conflict", want: "稳定 ID 冲突", edit: func(s *HerdrSnapshot) {
			conflict := s.Tabs[0]
			conflict.CWD = filepath.Join(root, "wrong")
			s.Tabs = append(s.Tabs, conflict)
		}},
		{name: "cross workspace same agent", want: "跨 workspace 同名", edit: func(s *HerdrSnapshot) { s.Agents[1].Name = "lichun" }},
		{name: "forged pane relationship", want: "关系无法证明", edit: func(s *HerdrSnapshot) {
			s.Tabs[0].CWD = filepath.Join(root, "engineering")
			s.Panes[0].TabID = "w-other:t1"
		}},
		{name: "agent tab pane mismatch", want: "关系无法证明", edit: func(s *HerdrSnapshot) { s.Agents[0].PaneID = "w-other:p1" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := cloneHerdrSnapshot(base)
			test.edit(&snapshot)
			err := decodeWorkspaceScopeSnapshot(t, snapshot)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v want substring %q", err, test.want)
			}
		})
	}
}

type workspaceScopeValidatingControl struct {
	*fakeHerdrControl
	raw []byte
}

func (c *workspaceScopeValidatingControl) Snapshot(_ context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	return decodeHerdrSnapshot(c.raw, scope)
}

func workspaceScopeControl(t *testing.T, root string, snapshot HerdrSnapshot) *workspaceScopeValidatingControl {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"result": map[string]any{"snapshot": snapshot}})
	if err != nil {
		t.Fatal(err)
	}
	return &workspaceScopeValidatingControl{fakeHerdrControl: newFakeHerdrControl(root, "hq-test"), raw: raw}
}

func TestWorkspaceScopeIdentityUpDoctorPatrolShareScopedSnapshotContract(t *testing.T) {
	e := setupTestEnv(t)
	cfg := testConfig()
	cwd := testAgentCWD(cfg, e.root, "penny")
	snapshot := HerdrSnapshot{
		Workspaces: []HerdrWorkspace{{ID: "w-target", Label: "hq-test"}, {ID: "w-other", Label: "other"}},
		Tabs: []HerdrTab{
			{ID: "w-target:t1", WorkspaceID: "w-target", Label: "Penny通报", CWD: cwd},
			{ID: "w-other:t1", WorkspaceID: "w-other", Label: "legacy"},
		},
		Panes:  []HerdrPane{{ID: "w-target:p1", WorkspaceID: "w-target", TabID: "w-target:t1", CWD: cwd}},
		Agents: []HerdrAgent{{Name: "penny", Kind: "codex", Status: "working", CWD: cwd, WorkspaceID: "w-target", TabID: "w-target:t1", PaneID: "w-target:p1", InteractiveReady: true}},
	}

	control := workspaceScopeControl(t, e.root, snapshot)
	if _, err := (herdrIdentityProvider{Control: control}).Resolve(cfg, e.root, "w-target:p1"); err != nil {
		t.Fatalf("identity rejected isolated unrelated cwd: %v", err)
	}
	if _, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, e.root, 0); err != nil {
		t.Fatalf("patrol rejected isolated unrelated cwd: %v", err)
	}
	if err := (controlDoctorRunner{Control: control}).WorkspaceList("", cfg.WorkspaceLabel); err != nil {
		t.Fatalf("doctor rejected isolated unrelated cwd: %v", err)
	}
	app := e.app(t)
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "workspace-scope-sessions")}
	app.Out, app.Err = io.Discard, io.Discard
	if err := app.runUp([]string{"--no-gateway"}); err != nil {
		t.Fatalf("up rejected isolated unrelated cwd: %v", err)
	}

	bad := cloneHerdrSnapshot(snapshot)
	bad.Panes[0].CWD = ""
	badControl := workspaceScopeControl(t, e.root, bad)
	if _, err := (herdrIdentityProvider{Control: badControl}).Resolve(cfg, e.root, "w-target:p1"); err == nil {
		t.Fatal("identity accepted target missing cwd")
	}
	if _, err := (&PatrolService{Herdr: badControl}).Run(context.Background(), cfg, e.root, 0); err == nil {
		t.Fatal("patrol accepted target missing cwd")
	}
	if err := (controlDoctorRunner{Control: badControl}).WorkspaceList("", cfg.WorkspaceLabel); err == nil {
		t.Fatal("doctor accepted target missing cwd")
	}
	app.Herdr = badControl
	if err := app.runUp([]string{"--no-gateway"}); err == nil {
		t.Fatal("up accepted target missing cwd")
	}
}

func TestWorkspaceScopeIdentityRejectsWrongTargetCWD(t *testing.T) {
	e := setupTestEnv(t)
	snapshot := workspaceScopeSnapshot(e.root)
	snapshot.Agents[0].Name = "eng-developer"
	wrong := filepath.Join(e.root, "product")
	snapshot.Tabs[0].CWD, snapshot.Panes[0].CWD, snapshot.Agents[0].CWD = wrong, wrong, wrong
	control := workspaceScopeControl(t, e.root, snapshot)
	if _, err := (herdrIdentityProvider{Control: control}).Resolve(testConfig(), e.root, "w-target:p1"); err == nil || !strings.Contains(err.Error(), "不在登记工位") {
		t.Fatalf("wrong target cwd not rejected: %v", err)
	}
}

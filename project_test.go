package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func projectTestCase(id, project, parent, status, priority, owner string) *CaseState {
	root := ""
	if parent != "" {
		root = "ALPHA-ROOT"
	}
	return &CaseState{
		ID: id, Project: project, ParentCaseID: parent, RootCaseID: root,
		Title: id + " title", Status: status, Priority: priority, Owner: owner,
		NextAction: "continue " + id,
	}
}

func projectDetailByName(t *testing.T, projection projectProjection, name string) ProjectDetailView {
	t.Helper()
	for _, project := range projection.Projects {
		if project.Summary.Project == name {
			return project
		}
	}
	t.Fatalf("project %q not found: %+v", name, projection.Projects)
	return ProjectDetailView{}
}

func mustBuildProjectProjection(t *testing.T, snapshot Snapshot, cfg Config) projectProjection {
	t.Helper()
	projection, err := buildProjectProjection(snapshot, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func TestProjectProjectionAggregatesBusinessStateAndGaps(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.GeneratedAt = "2026-09-01T09:00:00Z"
	snapshot.EventCount = 42
	snapshot.Cases = map[string]*CaseState{
		"ALPHA-ROOT":    projectTestCase("ALPHA-ROOT", "Alpha", "", string(statusOpen), "P1", "zantianyou"),
		"ALPHA-REVIEW":  projectTestCase("ALPHA-REVIEW", "Alpha", "ALPHA-ROOT", string(statusReported), "P0", "zantianyou"),
		"ALPHA-BLOCKED": projectTestCase("ALPHA-BLOCKED", "Alpha", "ALPHA-ROOT", string(statusNeedsDecision), "P2", "eng-developer"),
		"ALPHA-CLOSED":  projectTestCase("ALPHA-CLOSED", "Alpha", "ALPHA-ROOT", string(statusClosed), "P0", "penny"),
	}

	projection := mustBuildProjectProjection(t, snapshot, testConfig())
	if len(projection.Projects) != 1 {
		t.Fatalf("projection=%+v", projection)
	}
	if projection.Projects[0].Summary.Project != "Alpha" {
		t.Fatalf("single project missing: %+v", projection.Projects)
	}

	alpha := projectDetailByName(t, projection, "Alpha")
	summary := alpha.Summary
	if summary.Status != "blocked" || summary.Priority != "P0" || summary.RootCaseCount != 1 || summary.TotalCaseCount != 4 {
		t.Fatalf("alpha headline=%+v", summary)
	}
	if summary.ReviewGapCount != 1 || summary.BlockedGapCount != 1 || summary.ClosureGapCount != 3 {
		t.Fatalf("alpha gaps=%+v", summary)
	}
	if summary.StatusCounts[string(statusOpen)] != 1 || summary.StatusCounts[string(statusReported)] != 1 ||
		summary.StatusCounts[string(statusNeedsDecision)] != 1 || summary.StatusCounts[string(statusClosed)] != 1 {
		t.Fatalf("alpha status counts=%v", summary.StatusCounts)
	}
	if !reflect.DeepEqual(summary.PriorityCounts, map[string]int{"P0": 2, "P1": 1, "P2": 1, "unset": 0}) {
		t.Fatalf("alpha priority counts=%v", summary.PriorityCounts)
	}
	if !reflect.DeepEqual(summary.Owners, []ProjectOwnerCount{{Owner: "eng-developer", CaseCount: 1}, {Owner: "penny", CaseCount: 1}, {Owner: "zantianyou", CaseCount: 2}}) {
		t.Fatalf("alpha owners=%+v", summary.Owners)
	}
	if !reflect.DeepEqual(summary.Departments, []ProjectDepartmentCount{{Department: "ceo-office", CaseCount: 1}, {Department: "engineering", CaseCount: 3}}) {
		t.Fatalf("alpha departments=%+v", summary.Departments)
	}
	// Project status is derived only from durable case states. This fixture has
	// no Herdr runtime input, so a runtime "done" value cannot close a project.
	if alpha.Summary.ClosureGapCount == 0 {
		t.Fatal("open business work was inferred complete")
	}
}

func TestProjectProjectionFiltersWithoutChangingProjectTotals(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Cases = map[string]*CaseState{
		"ALPHA": projectTestCase("ALPHA", "Alpha", "", string(statusBlocked), "P0", "eng-developer"),
	}
	projection := mustBuildProjectProjection(t, snapshot, testConfig())

	tests := []struct {
		name    string
		filters projectFilters
		want    string
	}{
		{name: "status", filters: projectFilters{Status: "blocked"}, want: "Alpha"},
		{name: "priority", filters: projectFilters{Priority: "P0"}, want: "Alpha"},
		{name: "owner", filters: projectFilters{Owner: "eng-developer"}, want: "Alpha"},
		{name: "department", filters: projectFilters{Department: "engineering"}, want: "Alpha"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := projectListView(projection, test.filters)
			if view.ProjectCount != 1 || view.AssignedCaseCount != 1 || view.Projects[0].Project != test.want {
				t.Fatalf("view=%+v", view)
			}
		})
	}
	view := projectListView(projection, projectFilters{Status: "blocked", Department: "engineering"})
	if view.ProjectCount != 1 || view.Projects[0].StatusCounts[string(statusClosed)] != 0 {
		t.Fatalf("filter changed project aggregation: %+v", view)
	}
	if normalized, err := normalizeProjectFilters(projectFilters{Status: " REVIEW ", Priority: " p0 "}); err != nil || normalized.Status != "review" || normalized.Priority != "P0" {
		t.Fatalf("normalized=%+v err=%v", normalized, err)
	}
	for _, filters := range []projectFilters{{Status: "done"}, {Priority: "urgent"}} {
		if _, err := normalizeProjectFilters(filters); err == nil {
			t.Fatalf("invalid filters accepted: %+v", filters)
		}
	}
	first := renderProjectList(projectListView(projection, projectFilters{}))
	second := renderProjectList(projectListView(projection, projectFilters{}))
	if first != second || !strings.Contains(first, "Alpha status=") {
		t.Fatalf("unstable text:\n%s\n---\n%s", first, second)
	}
}

func TestProjectProjectionCountsEveryCaseStatus(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Cases = map[string]*CaseState{
		"ALPHA-ROOT": projectTestCase("ALPHA-ROOT", "Status Matrix", "", string(statusOpen), "P1", "penny"),
	}
	for _, status := range projectCaseStatusOrder[1:] {
		id := "STATUS-" + strings.ToUpper(status)
		snapshot.Cases[id] = projectTestCase(id, "Status Matrix", "ALPHA-ROOT", status, "P1", "penny")
	}
	summary := projectDetailByName(t, mustBuildProjectProjection(t, snapshot, testConfig()), "Status Matrix").Summary
	for _, status := range projectCaseStatusOrder {
		if summary.StatusCounts[status] != 1 {
			t.Fatalf("status=%s counts=%v", status, summary.StatusCounts)
		}
	}
	if summary.TotalCaseCount != len(projectCaseStatusOrder) || summary.RootCaseCount != 1 ||
		summary.ReviewGapCount != 3 || summary.BlockedGapCount != 3 || summary.ClosureGapCount != len(projectCaseStatusOrder)-1 || summary.Status != "blocked" {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestProjectProjectionRejectsMultipleRootsOrProjects(t *testing.T) {
	tests := []struct {
		name     string
		cases    map[string]*CaseState
		wantText string
	}{
		{
			name: "multiple roots and projects",
			cases: map[string]*CaseState{
				"ROOT-A": projectTestCase("ROOT-A", "Alpha", "", string(statusOpen), "P1", "penny"),
				"ROOT-B": projectTestCase("ROOT-B", "Beta", "", string(statusOpen), "P1", "penny"),
			},
			wantText: "恰有一个 root case",
		},
		{
			name: "empty project",
			cases: map[string]*CaseState{
				"ROOT": projectTestCase("ROOT", "", "", string(statusOpen), "P1", "penny"),
			},
			wantText: "必须冻结非空 project",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := newSnapshot()
			snapshot.Cases = test.cases
			if _, err := buildProjectProjection(snapshot, testConfig()); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("invalid project projection accepted: %v", err)
			}
		})
	}
}

func runProjectTestCommand(t *testing.T, e testEnv, jsonOutput bool, args ...string) (string, error) {
	t.Helper()
	app := e.app(t)
	var out, errOut bytes.Buffer
	app.JSON, app.Out, app.Err = jsonOutput, &out, &errOut
	err := app.run(args)
	if err != nil {
		return out.String(), err
	}
	if errOut.Len() != 0 {
		t.Fatalf("command %v wrote stderr: %s", args, errOut.String())
	}
	return out.String(), nil
}

func createProjectTestCase(t *testing.T, e testEnv, id, project, priority string, parent ...string) {
	t.Helper()
	args := []string{"case", "create", "--id", id, "--title", id,
		"--objective", "完成目标", "--acceptance", "结果可复验", "--constraints", "遵守岗位边界",
		"--priority", priority, "--source", writeTestFile(t, filepath.Join(e.office, id+".md"), "# "+id+"\n")}
	if project != "" && len(parent) == 0 {
		args = append(args, "--project", project)
	}
	if len(parent) != 0 {
		args = append(args, "--parent", parent[0])
	}
	runTestCommand(t, e, args...)
}

func TestProjectQueriesFailClosedOnTxnResidueWithoutWrites(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "list", args: []string{"project", "list"}},
		{name: "show", args: []string{"project", "show", "--project", "pending"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			txnDir := filepath.Join(e.data, "txn")
			if err := os.MkdirAll(txnDir, 0o700); err != nil {
				t.Fatal(err)
			}
			intentPath := filepath.Join(txnDir, "pending.json")
			want := []byte("durable pending project intent must remain untouched\n")
			if err := os.WriteFile(intentPath, want, 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := runProjectTestCommand(t, e, true, test.args...)
			if err == nil || !strings.Contains(err.Error(), "待恢复 txn intent") {
				t.Fatalf("project %s did not fail closed on txn residue: %v", test.name, err)
			}
			got, readErr := os.ReadFile(intentPath)
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("project %s changed txn intent: got=%q err=%v", test.name, got, readErr)
			}
			for _, path := range []string{filepath.Join(e.data, ".hq.lock"), filepath.Join(e.data, "state.json")} {
				if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
					t.Fatalf("project %s created read-only artifact %s: %v", test.name, path, statErr)
				}
			}
		})
	}
}

func TestProjectCommandsJSONTextNoProjectAndProjectionRebuild(t *testing.T) {
	t.Run("empty ledger", func(t *testing.T) {
		e := setupTestEnv(t)
		if _, err := os.Stat(e.data); !os.IsNotExist(err) {
			t.Fatalf("unexpected records before read: %v", err)
		}
		output, err := runProjectTestCommand(t, e, true, "project", "list")
		if err != nil {
			t.Fatal(err)
		}
		var empty ProjectListView
		if err := json.Unmarshal([]byte(output), &empty); err != nil {
			t.Fatal(err)
		}
		if empty.ProjectCount != 0 || empty.AssignedCaseCount != 0 || empty.Projects == nil {
			t.Fatalf("empty=%+v json=%s", empty, output)
		}
		if _, err := os.Stat(e.data); !os.IsNotExist(err) {
			t.Fatalf("read-only project list created records: %v", err)
		}

		if _, err := runProjectTestCommand(t, e, false, "project", "show", "--project", "missing"); err == nil || !strings.Contains(err.Error(), "project 不存在") {
			t.Fatalf("missing project err=%v", err)
		}
	})

	t.Run("stable CLI and state rebuild", func(t *testing.T) {
		e := setupTestEnv(t)
		e.setActor(t, "zantianyou", "project:engineering", filepath.Join(e.root, "engineering"))
		createProjectTestCase(t, e, "PROJECT-ALPHA-ROOT", "Alpha", "P0")
		createProjectTestCase(t, e, "PROJECT-ALPHA-CHILD", "Alpha", "P1", "PROJECT-ALPHA-ROOT")

		before, err := runProjectTestCommand(t, e, true, "project", "list")
		if err != nil {
			t.Fatal(err)
		}
		var list ProjectListView
		if err := json.Unmarshal([]byte(before), &list); err != nil {
			t.Fatal(err)
		}
		if list.ProjectCount != 1 || list.AssignedCaseCount != 2 ||
			list.Projects[0].Project != "Alpha" {
			t.Fatalf("list=%+v", list)
		}

		filtered, err := runProjectTestCommand(t, e, true, "project", "list", "--department", "engineering", "--priority", "p0")
		if err != nil {
			t.Fatal(err)
		}
		var engineering ProjectListView
		if err := json.Unmarshal([]byte(filtered), &engineering); err != nil {
			t.Fatal(err)
		}
		if engineering.ProjectCount != 1 || engineering.Projects[0].Project != "Alpha" || engineering.Projects[0].TotalCaseCount != 2 {
			t.Fatalf("engineering=%+v", engineering)
		}

		show, err := runProjectTestCommand(t, e, true, "project", "show", "--project", "Alpha")
		if err != nil {
			t.Fatal(err)
		}
		var detail ProjectDetailView
		if err := json.Unmarshal([]byte(show), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Summary.RootCaseCount != 1 || len(detail.Cases) != 2 || detail.Cases[0].CaseID != "PROJECT-ALPHA-CHILD" || detail.Cases[0].Department != "engineering" {
			t.Fatalf("detail=%+v", detail)
		}

		textBefore, err := runProjectTestCommand(t, e, false, "project", "show", "--project", "Alpha")
		if err != nil {
			t.Fatal(err)
		}
		statePath := filepath.Join(e.data, "state.json")
		if err := os.WriteFile(statePath, []byte("{corrupt derived state}"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Project View replays the authoritative ledger and ignores a corrupt
		// derived state file; it also never consults Herdr runtime state.
		whileCorrupt, err := runProjectTestCommand(t, e, true, "project", "list")
		if err != nil || whileCorrupt != before {
			t.Fatalf("ledger projection changed with corrupt state: err=%v\nbefore=%s\nafter=%s", err, before, whileCorrupt)
		}
		if _, err := NewStore(e.data).Rebuild(testConfig()); err != nil {
			t.Fatal(err)
		}
		after, err := runProjectTestCommand(t, e, true, "project", "list")
		if err != nil || after != before {
			t.Fatalf("projection changed after rebuild: err=%v\nbefore=%s\nafter=%s", err, before, after)
		}
		textAfter, err := runProjectTestCommand(t, e, false, "project", "show", "--project", "Alpha")
		if err != nil || textAfter != textBefore {
			t.Fatalf("unstable text after rebuild: err=%v\nbefore=%s\nafter=%s", err, textBefore, textAfter)
		}
	})
}

func TestBoardDisplaysCasePriorityInsteadOfFindingSeverity(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "board-priority.md"), "# evidence\n")
	e.setActor(t, "zantianyou", "board:priority", filepath.Join(e.root, "engineering"))
	createProjectTestCase(t, e, "BOARD-PRIORITY-001", "Board", "P2")
	runTestCommand(t, e, "report", "--case", "BOARD-PRIORITY-001", "--result", "finding",
		"--severity", "P0", "--source", source, "--location", "board", "--verify", "inspect row", "--next", "review")

	output, err := runProjectTestCommand(t, e, false, "board")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "PRI") || strings.Contains(output, "SEV") {
		t.Fatalf("board header=%s", output)
	}
	var row string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "BOARD-PRIORITY-001") {
			row = line
		}
	}
	if row == "" || !strings.Contains(row, "P2") || strings.Contains(row, "P0") {
		t.Fatalf("board row must display case priority, not finding severity: %q\n%s", row, output)
	}
}

func TestProjectDocumentationDefinesReadOnlyBusinessProjection(t *testing.T) {
	for _, path := range []string{"README.md", "DESIGN.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		document := string(raw)
		for _, want := range []string{"hq project list", "hq project show", "case.project", "Herdr", "closure"} {
			if !strings.Contains(document, want) {
				t.Fatalf("%s missing Project View contract %q", path, want)
			}
		}
	}
}

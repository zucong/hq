package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaffListFiltersExactDirectReportsWithoutRuntime(t *testing.T) {
	e := setupTestEnv(t)
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-data-engineer" {
			cfg.Agents[index].Disabled = true
			finalizeTestSeatMutation(&cfg.Agents[index])
		}
	}
	writeConfigFixture(t, e.config, cfg)

	var textOut bytes.Buffer
	app := e.app(t)
	app.Out, app.Err = &textOut, &textOut
	if err := app.cmdStaffList([]string{"--reports-to", "zantianyou"}); err != nil {
		t.Fatal(err)
	}
	text := textOut.String()
	for _, column := range []string{"ACTIVE_WIP", "MAX_WIP", "AVAILABLE_WIP"} {
		if !strings.Contains(text, column) {
			t.Fatalf("staff list omitted unambiguous live-capacity column %s:\n%s", column, text)
		}
	}
	if !strings.Contains(text, "eng-developer") {
		t.Fatalf("filtered staff list omitted active direct report:\n%s", text)
	}
	for _, excluded := range []string{"eng-data-engineer", "penny", "zantianyou", "baogong"} {
		if strings.Contains(text, "\n"+excluded+" ") {
			t.Fatalf("filtered staff list unexpectedly included %s:\n%s", excluded, text)
		}
	}

	var jsonOut bytes.Buffer
	app = e.app(t)
	app.JSON, app.Out, app.Err = true, &jsonOut, &jsonOut
	if err := app.cmdStaffList([]string{"--reports-to=zantianyou", "--all"}); err != nil {
		t.Fatal(err)
	}
	var got []staffCapacityView
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("decode filtered JSON: %v\n%s", err, jsonOut.String())
	}
	if len(got) != 2 || got[0].Name != "eng-data-engineer" || !got[0].Disabled || got[1].Name != "eng-developer" {
		t.Fatalf("--reports-to/--all returned wrong stable selection: %+v", got)
	}
	if got[1].ActiveWIP != 0 || got[1].MaxWIP != 1 || got[1].AvailableWIP != 1 {
		t.Fatalf("idle seat capacity is misleading: %+v", got[1])
	}
}

func TestStaffListAndGetExposeLiveAssignmentCapacity(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "staff-capacity-source.md"), "# staff capacity\n")
	e.setActor(t, "zantianyou", "staff:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "STAFF-CAPACITY", "--title", "Live capacity", "--source", source)
	runTestCommand(t, e, "issue", "--case", "STAFF-CAPACITY", "--to", "eng-developer", "--next", "Hold this active assignment")

	var listOut bytes.Buffer
	app := e.app(t)
	app.JSON, app.Out, app.Err = true, &listOut, &listOut
	if err := app.cmdStaffList([]string{"--reports-to", "zantianyou"}); err != nil {
		t.Fatal(err)
	}
	var staff []staffCapacityView
	if err := json.Unmarshal(listOut.Bytes(), &staff); err != nil {
		t.Fatal(err)
	}
	var developer staffCapacityView
	for _, view := range staff {
		if view.Name == "eng-developer" {
			developer = view
		}
	}
	if developer.Name == "" || developer.ActiveWIP != 1 || developer.MaxWIP != 1 || developer.AvailableWIP != 0 {
		t.Fatalf("active assignment was not reflected in staff list: %+v", developer)
	}

	var getOut bytes.Buffer
	app = e.app(t)
	app.JSON, app.Out, app.Err = true, &getOut, &getOut
	if err := app.cmdStaffGet([]string{"--name", "eng-developer"}); err != nil {
		t.Fatal(err)
	}
	var got staffCapacityView
	if err := json.Unmarshal(getOut.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ActiveWIP != 1 || got.MaxWIP != 1 || got.AvailableWIP != 0 {
		t.Fatalf("staff get disagrees with live capacity: %+v", got)
	}
}

func TestStaffListReportsToFlagIsPublishedToManagers(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, &bytes.Buffer{}, &bytes.Buffer{})
	command, remaining, err := root.Find([]string{"staff", "list"})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("find staff list: command=%v remaining=%v err=%v", command, remaining, err)
	}
	flag := command.Flags().Lookup("reports-to")
	if flag == nil || !strings.Contains(flag.Usage, "agent slug") {
		t.Fatalf("staff list does not publish --reports-to agent-slug filter: %+v", flag)
	}

	manager := AgentRule{
		Name: "product-manager", DepartmentLabel: "产品部", Responsibilities: []string{"manager:product"},
		RoleCardID: "product-manager", RoleCardVersion: 1, WorkstationPath: "product/staff/product-manager/v1",
		ActivationPolicy: activationAlways, MaxWIP: 8,
	}
	manual := string(agentRoleCardManual("Example", "example-hq", manager))
	if !strings.Contains(manual, "hq staff list --reports-to product-manager") {
		t.Fatalf("generated manager role manual omits exact direct-report query:\n%s", manual)
	}
	handbook := string(companyAgentHandbook(initPlan{CompanyName: "Example", Owner: "ZC", Workspace: "example-hq"}))
	if !strings.Contains(handbook, "hq staff list --reports-to <自己的-agent-slug>") {
		t.Fatalf("generated company handbook omits direct-report query:\n%s", handbook)
	}

	readme, err := os.ReadFile(repositoryPath("README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "hq staff list --reports-to <manager-agent-slug>") {
		t.Fatal("README omits the manager direct-report query")
	}
}

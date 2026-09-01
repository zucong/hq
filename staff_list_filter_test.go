package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestStaffListFiltersExactDirectReportsWithoutRuntime(t *testing.T) {
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-data-engineer" {
			cfg.Agents[index].Disabled = true
		}
	}

	var textOut bytes.Buffer
	app := &App{Config: cfg, Out: &textOut, Err: &textOut}
	if err := app.cmdStaffList([]string{"--reports-to", "zantianyou"}); err != nil {
		t.Fatal(err)
	}
	text := textOut.String()
	if !strings.Contains(text, "eng-developer") {
		t.Fatalf("filtered staff list omitted active direct report:\n%s", text)
	}
	for _, excluded := range []string{"eng-data-engineer", "penny", "zantianyou", "baogong"} {
		if strings.Contains(text, "\n"+excluded+" ") {
			t.Fatalf("filtered staff list unexpectedly included %s:\n%s", excluded, text)
		}
	}

	var jsonOut bytes.Buffer
	app = &App{Config: cfg, JSON: true, Out: &jsonOut, Err: &jsonOut}
	if err := app.cmdStaffList([]string{"--reports-to=zantianyou", "--all"}); err != nil {
		t.Fatal(err)
	}
	var got []AgentRule
	if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
		t.Fatalf("decode filtered JSON: %v\n%s", err, jsonOut.String())
	}
	if len(got) != 2 || got[0].Name != "eng-data-engineer" || !got[0].Disabled || got[1].Name != "eng-developer" {
		t.Fatalf("--reports-to/--all returned wrong stable selection: %+v", got)
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

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "hq staff list --reports-to <manager-agent-slug>") {
		t.Fatal("README omits the manager direct-report query")
	}
}

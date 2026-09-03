package main

import (
	"fmt"
	"sort"
	"strings"
)

const projectViewVersion = 2

var projectCaseStatusOrder = []string{
	string(statusOpen),
	string(statusDispatched),
	string(statusInProgress),
	string(statusReported),
	string(statusBlocked),
	string(statusNeedsDecision),
	string(statusFindingReported),
	string(statusFindingAccepted),
	string(statusEscalated),
	string(statusAccepted),
	string(statusReturned),
	string(statusClosed),
}

var projectSummaryStatuses = map[string]bool{
	"active":  true,
	"review":  true,
	"blocked": true,
	"closed":  true,
}

type ProjectOwnerCount struct {
	Owner     string `json:"owner"`
	CaseCount int    `json:"case_count"`
}

type ProjectDepartmentCount struct {
	Department string `json:"department"`
	CaseCount  int    `json:"case_count"`
}

type ProjectCaseView struct {
	CaseID       string `json:"case_id"`
	ParentCaseID string `json:"parent_case_id,omitempty"`
	RootCaseID   string `json:"root_case_id,omitempty"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Priority     string `json:"priority"`
	Owner        string `json:"owner,omitempty"`
	Department   string `json:"department,omitempty"`
	NextAction   string `json:"next_action,omitempty"`
	IsRoot       bool   `json:"is_root"`
}

type ProjectSummary struct {
	Project         string                   `json:"project"`
	Status          string                   `json:"status"`
	Priority        string                   `json:"priority"`
	RootCaseCount   int                      `json:"root_case_count"`
	TotalCaseCount  int                      `json:"total_case_count"`
	ReviewGapCount  int                      `json:"review_gap_count"`
	BlockedGapCount int                      `json:"blocked_gap_count"`
	ClosureGapCount int                      `json:"closure_gap_count"`
	StatusCounts    map[string]int           `json:"status_counts"`
	PriorityCounts  map[string]int           `json:"priority_counts"`
	Owners          []ProjectOwnerCount      `json:"owners"`
	Departments     []ProjectDepartmentCount `json:"departments"`
}

type ProjectListView struct {
	Version           int              `json:"version"`
	GeneratedAt       string           `json:"generated_at"`
	EventCount        int              `json:"event_count"`
	ProjectCount      int              `json:"project_count"`
	AssignedCaseCount int              `json:"assigned_case_count"`
	Projects          []ProjectSummary `json:"projects"`
}

type ProjectDetailView struct {
	Version     int               `json:"version"`
	GeneratedAt string            `json:"generated_at"`
	EventCount  int               `json:"event_count"`
	Summary     ProjectSummary    `json:"summary"`
	Cases       []ProjectCaseView `json:"cases"`
}

type projectProjection struct {
	GeneratedAt string
	EventCount  int
	Projects    []ProjectDetailView
}

type projectFilters struct {
	Status     string
	Priority   string
	Owner      string
	Department string
}

func (a *App) cmdProject(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq project list|show ...")
	}
	switch args[0] {
	case "list":
		return a.cmdProjectList(args[1:])
	case "show":
		return a.cmdProjectShow(args[1:])
	default:
		return fmt.Errorf("未知 project 子命令 %q", args[0])
	}
}

func (a *App) projectSnapshot() (Snapshot, error) {
	if store, ok := a.Store.(interface {
		SnapshotReadOnly(Config) (Snapshot, error)
	}); ok {
		return store.SnapshotReadOnly(a.Config)
	}
	return a.Store.Snapshot(a.Config)
}

func (a *App) cmdProjectList(args []string) error {
	fs := newLeafParser("project list")
	fs.SetOutput(a.Err)
	status := fs.String("status", "", "active|review|blocked|closed")
	priority := fs.String("priority", "", "P0|P1|P2|unset")
	owner := fs.String("owner", "", "包含该 owner 的项目")
	department := fs.String("department", "", "包含该部门的项目")
	if err := fs.Parse(args); err != nil {
		return err
	}
	filters, err := normalizeProjectFilters(projectFilters{
		Status: *status, Priority: *priority, Owner: *owner, Department: *department,
	})
	if err != nil {
		return err
	}
	snapshot, err := a.projectSnapshot()
	if err != nil {
		return err
	}
	projection, err := buildProjectProjection(snapshot, a.Config)
	if err != nil {
		return err
	}
	view := projectListView(projection, filters)
	return a.output(view, renderProjectList(view))
}

func (a *App) cmdProjectShow(args []string) error {
	fs := newLeafParser("project show")
	fs.SetOutput(a.Err)
	project := fs.String("project", "", "case.project 的精确值")
	if err := fs.Parse(args); err != nil {
		return err
	}
	name := strings.TrimSpace(*project)
	if name == "" {
		return fmt.Errorf("缺少 --project")
	}
	snapshot, err := a.projectSnapshot()
	if err != nil {
		return err
	}
	projection, err := buildProjectProjection(snapshot, a.Config)
	if err != nil {
		return err
	}
	for _, candidate := range projection.Projects {
		if candidate.Summary.Project == name {
			return a.output(candidate, renderProjectDetail(candidate))
		}
	}
	return fmt.Errorf("project 不存在：%s", name)
}

func normalizeProjectFilters(filters projectFilters) (projectFilters, error) {
	filters.Status = strings.ToLower(strings.TrimSpace(filters.Status))
	filters.Priority = strings.ToUpper(strings.TrimSpace(filters.Priority))
	filters.Owner = strings.TrimSpace(filters.Owner)
	filters.Department = strings.TrimSpace(filters.Department)
	if filters.Status != "" && !projectSummaryStatuses[filters.Status] {
		return projectFilters{}, fmt.Errorf("--status 只能是 active/review/blocked/closed")
	}
	if filters.Priority != "" && filters.Priority != "P0" && filters.Priority != "P1" && filters.Priority != "P2" && filters.Priority != "UNSET" {
		return projectFilters{}, fmt.Errorf("--priority 只能是 P0/P1/P2/unset")
	}
	if filters.Priority == "UNSET" {
		filters.Priority = "unset"
	}
	return filters, nil
}

func buildProjectProjection(snapshot Snapshot, cfg Config) (projectProjection, error) {
	projection := projectProjection{GeneratedAt: snapshot.GeneratedAt, EventCount: snapshot.EventCount}
	strict := newLedgerState()
	strict.snapshot = snapshot
	if err := strict.validateCaseTreeFinalInvariants(); err != nil {
		return projectProjection{}, fmt.Errorf("project projection 拒绝非单项目账本：%w", err)
	}
	root := strict.soleRootCase()
	if root == nil {
		projection.Projects = []ProjectDetailView{}
		return projection, nil
	}
	cases := make([]ProjectCaseView, 0, len(snapshot.Cases))
	for _, state := range snapshot.Cases {
		department := ""
		if rule, ok := configRuleIncludingDisabled(cfg, state.Owner); ok {
			department = rule.Department
		}
		cases = append(cases, ProjectCaseView{
			CaseID: state.ID, ParentCaseID: state.ParentCaseID, RootCaseID: state.RootCaseID,
			Title: state.Title, Status: state.Status, Priority: normalizedProjectPriority(state.Priority),
			Owner: state.Owner, Department: department, NextAction: state.NextAction, IsRoot: state.ID == root.ID,
		})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].CaseID < cases[j].CaseID })
	projection.Projects = []ProjectDetailView{{
		Version: projectViewVersion, GeneratedAt: snapshot.GeneratedAt, EventCount: snapshot.EventCount,
		Summary: summarizeProject(root.Project, cases), Cases: cases,
	}}
	return projection, nil
}

func summarizeProject(name string, cases []ProjectCaseView) ProjectSummary {
	statusCounts := make(map[string]int, len(projectCaseStatusOrder))
	for _, status := range projectCaseStatusOrder {
		statusCounts[status] = 0
	}
	priorityCounts := map[string]int{"P0": 0, "P1": 0, "P2": 0, "unset": 0}
	ownerCounts := map[string]int{}
	departmentCounts := map[string]int{}
	activePriorityCounts := map[string]int{"P0": 0, "P1": 0, "P2": 0, "unset": 0}
	summary := ProjectSummary{
		Project: name, TotalCaseCount: len(cases), StatusCounts: statusCounts, PriorityCounts: priorityCounts,
		Owners: []ProjectOwnerCount{}, Departments: []ProjectDepartmentCount{},
	}
	for _, item := range cases {
		statusCounts[item.Status]++
		priorityCounts[item.Priority]++
		if item.Status != string(statusClosed) {
			activePriorityCounts[item.Priority]++
		}
		if item.Owner != "" {
			ownerCounts[item.Owner]++
		}
		if item.Department != "" {
			departmentCounts[item.Department]++
		}
		if item.IsRoot {
			summary.RootCaseCount++
		}
		switch item.Status {
		case string(statusReported), string(statusFindingReported), string(statusEscalated):
			summary.ReviewGapCount++
		case string(statusBlocked), string(statusNeedsDecision), string(statusReturned):
			summary.BlockedGapCount++
		}
		if item.Status != string(statusClosed) {
			summary.ClosureGapCount++
		}
	}
	summary.Status = projectSummaryStatus(summary)
	summary.Priority = highestProjectPriority(activePriorityCounts)
	if summary.Priority == "unset" && summary.ClosureGapCount == 0 {
		summary.Priority = highestProjectPriority(priorityCounts)
	}
	summary.Owners = sortedProjectOwners(ownerCounts)
	summary.Departments = sortedProjectDepartments(departmentCounts)
	return summary
}

func projectSummaryStatus(summary ProjectSummary) string {
	if summary.TotalCaseCount > 0 && summary.ClosureGapCount == 0 {
		return "closed"
	}
	if summary.BlockedGapCount > 0 {
		return "blocked"
	}
	if summary.ReviewGapCount > 0 {
		return "review"
	}
	return "active"
}

func normalizedProjectPriority(priority string) string {
	priority = strings.ToUpper(strings.TrimSpace(priority))
	if priority == "P0" || priority == "P1" || priority == "P2" {
		return priority
	}
	return "unset"
}

func highestProjectPriority(counts map[string]int) string {
	for _, priority := range []string{"P0", "P1", "P2"} {
		if counts[priority] > 0 {
			return priority
		}
	}
	return "unset"
}

func sortedProjectCountValues(counts map[string]int) []string {
	values := make([]string, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func sortedProjectOwners(counts map[string]int) []ProjectOwnerCount {
	values := sortedProjectCountValues(counts)
	result := make([]ProjectOwnerCount, 0, len(values))
	for _, value := range values {
		result = append(result, ProjectOwnerCount{Owner: value, CaseCount: counts[value]})
	}
	return result
}

func sortedProjectDepartments(counts map[string]int) []ProjectDepartmentCount {
	values := sortedProjectCountValues(counts)
	result := make([]ProjectDepartmentCount, 0, len(values))
	for _, value := range values {
		result = append(result, ProjectDepartmentCount{Department: value, CaseCount: counts[value]})
	}
	return result
}

func projectListView(projection projectProjection, filters projectFilters) ProjectListView {
	view := ProjectListView{
		Version: projectViewVersion, GeneratedAt: projection.GeneratedAt, EventCount: projection.EventCount,
		Projects: []ProjectSummary{},
	}
	for _, project := range projection.Projects {
		if !projectMatchesFilters(project.Summary, filters) {
			continue
		}
		view.Projects = append(view.Projects, project.Summary)
		view.AssignedCaseCount += project.Summary.TotalCaseCount
	}
	view.ProjectCount = len(view.Projects)
	return view
}

func projectMatchesFilters(summary ProjectSummary, filters projectFilters) bool {
	if filters.Status != "" && summary.Status != filters.Status {
		return false
	}
	if filters.Priority != "" && summary.Priority != filters.Priority {
		return false
	}
	if filters.Owner != "" && !projectOwnersContain(summary.Owners, filters.Owner) {
		return false
	}
	if filters.Department != "" && !projectDepartmentsContain(summary.Departments, filters.Department) {
		return false
	}
	return true
}

func projectOwnersContain(counts []ProjectOwnerCount, owner string) bool {
	for _, item := range counts {
		if item.Owner == owner {
			return true
		}
	}
	return false
}

func projectDepartmentsContain(counts []ProjectDepartmentCount, department string) bool {
	for _, item := range counts {
		if item.Department == department {
			return true
		}
	}
	return false
}

func renderProjectList(view ProjectListView) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "HQ 项目：%d project / %d cases / %d events", view.ProjectCount, view.AssignedCaseCount, view.EventCount)
	for _, project := range view.Projects {
		fmt.Fprintf(&builder, "\n%s status=%s priority=%s roots=%d total=%d review_gap=%d blocked_gap=%d closure_gap=%d owners=%s departments=%s",
			project.Project, project.Status, project.Priority, project.RootCaseCount, project.TotalCaseCount,
			project.ReviewGapCount, project.BlockedGapCount, project.ClosureGapCount,
			renderProjectOwners(project.Owners), renderProjectDepartments(project.Departments))
	}
	return builder.String()
}

func renderProjectDetail(view ProjectDetailView) string {
	var builder strings.Builder
	project := view.Summary
	fmt.Fprintf(&builder, "Project %s\nstatus=%s priority=%s roots=%d total=%d review_gap=%d blocked_gap=%d closure_gap=%d",
		project.Project, project.Status, project.Priority, project.RootCaseCount, project.TotalCaseCount,
		project.ReviewGapCount, project.BlockedGapCount, project.ClosureGapCount)
	fmt.Fprintf(&builder, "\nstatuses=%s", renderStatusCounts(project.StatusCounts))
	fmt.Fprintf(&builder, "\npriorities=%s", renderPriorityCounts(project.PriorityCounts))
	fmt.Fprintf(&builder, "\nowners=%s\ndepartments=%s", renderProjectOwners(project.Owners), renderProjectDepartments(project.Departments))
	for _, item := range view.Cases {
		parent := item.ParentCaseID
		if parent == "" {
			parent = "-"
		}
		fmt.Fprintf(&builder, "\ncase=%s status=%s priority=%s owner=%s department=%s parent=%s root=%t next=%s",
			item.CaseID, item.Status, item.Priority, item.Owner, item.Department, parent, item.IsRoot, item.NextAction)
	}
	return builder.String()
}

func renderProjectOwners(counts []ProjectOwnerCount) string {
	if len(counts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(counts))
	for _, item := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", item.Owner, item.CaseCount))
	}
	return strings.Join(parts, ",")
}

func renderProjectDepartments(counts []ProjectDepartmentCount) string {
	if len(counts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(counts))
	for _, item := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", item.Department, item.CaseCount))
	}
	return strings.Join(parts, ",")
}

func renderStatusCounts(counts map[string]int) string {
	parts := make([]string, 0, len(projectCaseStatusOrder))
	for _, status := range projectCaseStatusOrder {
		parts = append(parts, fmt.Sprintf("%s:%d", status, counts[status]))
	}
	return strings.Join(parts, ",")
}

func renderPriorityCounts(counts map[string]int) string {
	parts := make([]string, 0, 4)
	for _, priority := range []string{"P0", "P1", "P2", "unset"} {
		parts = append(parts, fmt.Sprintf("%s:%d", priority, counts[priority]))
	}
	return strings.Join(parts, ",")
}

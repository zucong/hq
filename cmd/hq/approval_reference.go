package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	approvalHeaderMarker  = "<!-- hq-approval:v1\n"
	referenceHeaderMarker = "<!-- hq-reference-root:v1\n"
	metadataHeaderEnd     = "\n-->\n"
	maxMetadataFileSize   = 1 << 20
)

var (
	gitReferencePattern = regexp.MustCompile(`^git:(.+)@([0-9a-fA-F]{7,64})$`)
	decisionIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$`)
)

type ApprovalMetadata struct {
	Version     int             `json:"version"`
	DecisionID  string          `json:"decision_id"`
	Status      string          `json:"status"`
	ConfirmedBy string          `json:"confirmed_by"`
	ConfirmedAt string          `json:"confirmed_at"`
	Scopes      []ApprovalScope `json:"scopes"`
}

type ApprovalScope struct {
	Action        string `json:"action"`
	CaseID        string `json:"case_id,omitempty"`
	SourceRef     string `json:"source_ref,omitempty"`
	Target        string `json:"target"`
	RequestDigest string `json:"request_digest,omitempty"`
}

type archiveReferenceMetadata struct {
	Version int    `json:"version"`
	Root    string `json:"root"`
}

type referencePolicy struct {
	HQRoot       string
	ProjectRoots []string
	BeforeOpen   func(string) error
}

type allowedReferenceRoot struct {
	lexical   string
	canonical string
}

func normalizeReference(value, hqRoot string, required bool) (string, error) {
	policy, err := defaultReferencePolicy(hqRoot)
	if err != nil {
		return "", err
	}
	return normalizeRefWithPolicy(value, policy, required)
}

func defaultReferencePolicy(hqRoot string) (referencePolicy, error) {
	projects, err := discoverProjectReferenceRoots(hqRoot)
	if err != nil {
		return referencePolicy{}, err
	}
	return referencePolicy{HQRoot: hqRoot, ProjectRoots: projects}, nil
}

func discoverProjectReferenceRoots(hqRoot string) ([]string, error) {
	archives := filepath.Join(hqRoot, "_archives")
	info, err := os.Lstat(archives)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("_archives 必须是非 symlink 目录：%s", archives)
	}
	entries, err := os.ReadDir(archives)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var roots []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(archives, entry.Name())
		entryInfo, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
			return nil, fmt.Errorf("档案登记必须是非 symlink 普通文件：%s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(string(raw), referenceHeaderMarker) {
			continue
		}
		block, err := parseMetadataHeader(raw, referenceHeaderMarker, "hq-reference-root")
		if err != nil {
			return nil, fmt.Errorf("档案引用根元数据 %s：%w", path, err)
		}
		var metadata archiveReferenceMetadata
		if err := decodeStrictJSON(block, &metadata); err != nil {
			return nil, fmt.Errorf("档案引用根元数据 %s：%w", path, err)
		}
		if metadata.Version != 1 || !filepath.IsAbs(metadata.Root) {
			return nil, fmt.Errorf("档案引用根必须是 version=1 的绝对路径：%s", path)
		}
		root, err := canonicalReferenceDirectory(metadata.Root)
		if err != nil {
			return nil, fmt.Errorf("档案引用根 %s：%w", path, err)
		}
		canonicalHQ, err := filepath.EvalSymlinks(filepath.Clean(hqRoot))
		if err != nil {
			return nil, err
		}
		if root == string(filepath.Separator) || pathWithin(canonicalHQ, root) {
			return nil, fmt.Errorf("档案引用根过宽，不能包含总部根或整个卷：%s", root)
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots, nil
}

func normalizeRefWithPolicy(value string, policy referencePolicy, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("缺少文件/提交引用")
	}
	if value == "" {
		return "", nil
	}
	if containsSensitive(value) {
		return "", fmt.Errorf("引用疑似指向敏感路径或包含金额")
	}
	roots, err := resolveReferenceRoots(policy)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(value, "git:") {
		return normalizeGitRef(value, roots)
	}
	if strings.Count(value, "#") > 1 {
		return "", fmt.Errorf("引用 fragment 歧义：只允许一个 #定位")
	}
	pathPart, fragment, hasFragment := strings.Cut(value, "#")
	if hasFragment {
		fragment = strings.TrimSpace(fragment)
		if fragment == "" || strings.ContainsAny(fragment, "\r\n") {
			return "", fmt.Errorf("引用 fragment 必须是非空单行定位文本")
		}
	}
	pathPart = strings.TrimPrefix(pathPart, "file:")
	if !filepath.IsAbs(pathPart) {
		pathPart = filepath.Join(policy.HQRoot, pathPart)
	}
	file, canonical, err := openAllowedRegularFile(pathPart, roots, policy.BeforeOpen)
	if err != nil {
		return "", err
	}
	_ = file.Close()
	if hasFragment {
		return canonical + "#" + fragment, nil
	}
	return canonical, nil
}

func resolveReferenceRoots(policy referencePolicy) ([]allowedReferenceRoot, error) {
	candidates := append([]string{policy.HQRoot}, policy.ProjectRoots...)
	seen := map[string]bool{}
	roots := make([]allowedReferenceRoot, 0, len(candidates))
	for _, candidate := range candidates {
		lexical, err := filepath.Abs(candidate)
		if err != nil {
			return nil, err
		}
		lexical = filepath.Clean(lexical)
		canonical, err := canonicalReferenceDirectory(lexical)
		if err != nil {
			return nil, err
		}
		if !seen[canonical] {
			seen[canonical] = true
			roots = append(roots, allowedReferenceRoot{lexical: lexical, canonical: canonical})
		}
	}
	return roots, nil
}

func canonicalReferenceDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("引用根/仓库必须是非 symlink 目录：%s", abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(canonical), nil
}

func openAllowedRegularFile(path string, roots []allowedReferenceRoot, beforeOpen func(string) error) (*os.File, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	abs = filepath.Clean(abs)
	initial, err := os.Lstat(abs)
	if err != nil {
		return nil, "", fmt.Errorf("引用路径不存在：%s", abs)
	}
	if !isRegularReferenceMode(initial.Mode()) {
		return nil, "", fmt.Errorf("引用必须是非 symlink 普通文件：%s", abs)
	}
	if beforeOpen != nil {
		if err := beforeOpen(abs); err != nil {
			return nil, "", err
		}
	}
	fd, err := syscall.Open(abs, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, "", fmt.Errorf("安全打开引用失败：%s：%w", abs, err)
	}
	file := os.NewFile(uintptr(fd), abs)
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, "", err
	}
	final, err := os.Lstat(abs)
	if err != nil || !isRegularReferenceMode(final.Mode()) || !os.SameFile(opened, final) {
		file.Close()
		return nil, "", fmt.Errorf("引用路径在校验期间被替换：%s", abs)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		file.Close()
		return nil, "", err
	}
	matched := false
	for _, root := range roots {
		if pathWithin(canonical, root.canonical) {
			if err := rejectSymlinkBelowRoot(abs, canonical, root); err != nil {
				file.Close()
				return nil, "", err
			}
			matched = true
			break
		}
	}
	if !matched {
		file.Close()
		return nil, "", fmt.Errorf("引用路径不在总部或档案登记项目根内：%s", canonical)
	}
	return file, filepath.Clean(canonical), nil
}

func isRegularReferenceMode(mode os.FileMode) bool {
	return mode&os.ModeSymlink == 0 && mode.IsRegular()
}

func rejectSymlinkBelowRoot(abs, canonical string, root allowedReferenceRoot) error {
	base, candidate := root.lexical, abs
	if !pathWithin(candidate, base) {
		base, candidate = root.canonical, canonical
	}
	relative, err := filepath.Rel(base, candidate)
	if err != nil || relative == "." {
		return err
	}
	current := base
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("引用路径含 symlink：%s", current)
		}
	}
	return nil
}

func normalizeGitRef(value string, roots []allowedReferenceRoot) (string, error) {
	match := gitReferencePattern.FindStringSubmatch(value)
	if match == nil {
		return "", fmt.Errorf("git 引用必须是 git:/absolute/repo@<7-64位十六进制commit>")
	}
	repo, sha := match[1], strings.ToLower(match[2])
	if !filepath.IsAbs(repo) {
		return "", fmt.Errorf("git 仓库路径必须是绝对路径")
	}
	canonical, err := canonicalReferenceDirectory(repo)
	if err != nil {
		return "", fmt.Errorf("git 引用仓库无效：%w", err)
	}
	allowed := false
	for _, root := range roots {
		if pathWithin(canonical, root.canonical) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("git 仓库不在总部或档案登记项目根内：%s", canonical)
	}
	environment := append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	revParse := exec.Command("git", "-c", "core.hooksPath=/dev/null", "-C", canonical, "rev-parse", "--show-toplevel")
	revParse.Env = environment
	rootOutput, err := revParse.Output()
	if err != nil {
		return "", fmt.Errorf("git 引用不是有效仓库根：%s", canonical)
	}
	reportedRoot := strings.TrimSpace(string(rootOutput))
	reportedCanonical, err := filepath.EvalSymlinks(reportedRoot)
	if err != nil || filepath.Clean(reportedCanonical) != canonical {
		return "", fmt.Errorf("git 引用必须指向 canonical repo 根：%s", canonical)
	}
	catFile := exec.Command("git", "-c", "core.hooksPath=/dev/null", "-C", canonical, "cat-file", "-e", sha+"^{commit}")
	catFile.Env = environment
	if err := catFile.Run(); err != nil {
		return "", fmt.Errorf("git commit 不存在或不是 commit：%s@%s", canonical, sha)
	}
	return "git:" + canonical + "@" + sha, nil
}

func validateApprovalReference(value, office, ownerPrincipal string) (string, error) {
	ref, _, err := readApproval(value, office, ownerPrincipal, true)
	return ref, err
}

func validateApprovalScope(value, office, ownerPrincipal string, expected ApprovalScope) (string, error) {
	ref, metadata, err := readApproval(value, office, ownerPrincipal, true)
	if err != nil {
		return "", err
	}
	for _, scope := range metadata.Scopes {
		if scope == expected {
			return ref, nil
		}
	}
	return "", fmt.Errorf("批准 %s 未授权精确 scope：action=%s target=%s case=%s source=%s digest=%s", metadata.DecisionID, expected.Action, expected.Target, expected.CaseID, expected.SourceRef, expected.RequestDigest)
}

func readApproval(value, office, ownerPrincipal string, checkUnique bool) (string, ApprovalMetadata, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ApprovalMetadata{}, fmt.Errorf("缺少批准文件")
	}
	if strings.Contains(value, "#") || strings.HasPrefix(value, "git:") {
		return "", ApprovalMetadata{}, fmt.Errorf("批准文件不接受 fragment 或 git 引用")
	}
	value = strings.TrimPrefix(value, "file:")
	if !filepath.IsAbs(value) {
		value = filepath.Join(filepath.Dir(office), value)
	}
	decisions := filepath.Join(office, "decisions")
	roots, err := resolveReferenceRoots(referencePolicy{HQRoot: decisions})
	if err != nil {
		return "", ApprovalMetadata{}, err
	}
	file, canonical, err := openAllowedRegularFile(value, roots, nil)
	if err != nil {
		return "", ApprovalMetadata{}, fmt.Errorf("自动传令只接受 decisions 内的非 symlink 普通批准文件：%w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxMetadataFileSize+1))
	if err != nil {
		return "", ApprovalMetadata{}, err
	}
	if len(raw) > maxMetadataFileSize {
		return "", ApprovalMetadata{}, fmt.Errorf("批准文件超过 %d bytes", maxMetadataFileSize)
	}
	metadata, err := parseApprovalMetadata(raw)
	if err != nil {
		return "", ApprovalMetadata{}, fmt.Errorf("批准文件 %s：%w", canonical, err)
	}
	if metadata.ConfirmedBy != ownerPrincipal {
		return "", ApprovalMetadata{}, fmt.Errorf("批准文件 %s：confirmed_by 必须精确匹配 owner_principal %q", canonical, ownerPrincipal)
	}
	if checkUnique {
		if err := ensureUniqueDecisionID(filepath.Dir(canonical), canonical, metadata.DecisionID); err != nil {
			return "", ApprovalMetadata{}, err
		}
	}
	return canonical, metadata, nil
}

func parseApprovalMetadata(raw []byte) (ApprovalMetadata, error) {
	block, err := parseMetadataHeader(raw, approvalHeaderMarker, "hq-approval")
	if err != nil {
		return ApprovalMetadata{}, err
	}
	var metadata ApprovalMetadata
	if err := decodeStrictJSON(block, &metadata); err != nil {
		return ApprovalMetadata{}, err
	}
	if metadata.Version != 1 {
		return ApprovalMetadata{}, fmt.Errorf("未知批准元数据版本 %d", metadata.Version)
	}
	if !decisionIDPattern.MatchString(metadata.DecisionID) {
		return ApprovalMetadata{}, fmt.Errorf("decision_id 无效")
	}
	if metadata.Status != "effective" {
		return ApprovalMetadata{}, fmt.Errorf("批准状态必须精确为 effective")
	}
	if err := validateOwnerPrincipal(metadata.ConfirmedBy); err != nil {
		return ApprovalMetadata{}, fmt.Errorf("confirmed_by：%w", err)
	}
	if _, err := time.Parse(time.RFC3339, metadata.ConfirmedAt); err != nil {
		return ApprovalMetadata{}, fmt.Errorf("confirmed_at 必须是 RFC3339：%w", err)
	}
	if len(metadata.Scopes) == 0 {
		return ApprovalMetadata{}, fmt.Errorf("scopes 至少包含一项")
	}
	seen := map[ApprovalScope]bool{}
	for _, scope := range metadata.Scopes {
		if err := validateApprovalScopeShape(scope); err != nil {
			return ApprovalMetadata{}, err
		}
		if seen[scope] {
			return ApprovalMetadata{}, fmt.Errorf("重复批准 scope")
		}
		seen[scope] = true
	}
	return metadata, nil
}

func validateApprovalScopeShape(scope ApprovalScope) error {
	if scope.Action == "role:add" || scope.Action == "role:retire" {
		if _, _, err := parseRoleCardRef(scope.Target); err != nil {
			return fmt.Errorf("批准 scope role target 无效：%s：%w", scope.Target, err)
		}
	} else if !agentNamePattern.MatchString(scope.Target) {
		return fmt.Errorf("批准 scope target 无效：%s", scope.Target)
	}
	switch scope.Action {
	case "issue":
		if validateCaseID(scope.CaseID) != nil || scope.SourceRef == "" || scope.RequestDigest != "" || strings.ContainsAny(scope.SourceRef, "\r\n") {
			return fmt.Errorf("issue scope 必须精确包含 action/case_id/source_ref/target")
		}
	case "staff:add", "staff:update", "staff:remove":
		if scope.CaseID != "" || scope.SourceRef != "" || !sha256Pattern.MatchString(scope.RequestDigest) {
			return fmt.Errorf("staff scope 必须精确包含 action/target/request_digest")
		}
	case "role:add", "role:retire":
		if scope.CaseID != "" || scope.SourceRef != "" || !sha256Pattern.MatchString(scope.RequestDigest) {
			return fmt.Errorf("role scope 必须精确包含 action/target/request_digest")
		}
	case "company:init":
		if scope.CaseID != "" || scope.SourceRef != "" || !sha256Pattern.MatchString(scope.RequestDigest) {
			return fmt.Errorf("company:init scope 必须精确包含 action/target/request_digest")
		}
	default:
		return fmt.Errorf("未知批准 scope action：%s", scope.Action)
	}
	return nil
}

func parseMetadataHeader(raw []byte, marker, label string) ([]byte, error) {
	text := string(raw)
	if !strings.HasPrefix(text, marker) {
		return nil, fmt.Errorf("Markdown 必须从唯一 %s 元数据块开始（拒绝 BOM/前置正文）", label)
	}
	if strings.Count(text, "<!-- "+label+":") != 1 {
		return nil, fmt.Errorf("%s 元数据块必须唯一", label)
	}
	remainder := text[len(marker):]
	end := strings.Index(remainder, metadataHeaderEnd)
	if end < 0 {
		return nil, fmt.Errorf("%s 元数据块未闭合", label)
	}
	return []byte(remainder[:end]), nil
}

func ensureUniqueDecisionID(directory, selectedPath, decisionID string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if path == selectedPath || entry.IsDir() {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(raw), approvalHeaderMarker) {
			continue
		}
		other, err := parseApprovalMetadata(raw)
		if err != nil {
			return fmt.Errorf("同目录结构化批准无效 %s：%w", path, err)
		}
		if other.DecisionID == decisionID {
			return fmt.Errorf("decision_id %s 与 %s 重复", decisionID, path)
		}
	}
	return nil
}

func staffScopeDigest(action string, rule AgentRule) string {
	rule.ApprovalRef, rule.UpdatedAt = "", ""
	request := struct {
		Action string    `json:"action"`
		Target string    `json:"target"`
		Rule   AgentRule `json:"rule"`
	}{Action: action, Target: rule.Name, Rule: rule}
	return canonicalJSONDigest(request)
}

func canonicalJSONDigest(value any) string {
	raw, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	_ = decoder.Decode(&document)
	canonical, _ := json.Marshal(document)
	return digestText(string(canonical))
}

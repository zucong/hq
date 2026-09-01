package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

func workstationReference(rule AgentRule) string {
	return strings.TrimSpace(rule.WorkstationPath)
}

// resolveAgentWorkstation resolves the one authoritative cwd for an HQ seat.
// Registry paths are portable, workspace-relative references; runtime binding
// always uses the canonical absolute directory and rejects symlink aliases.
func resolveAgentWorkstation(hqRoot string, rule AgentRule) (string, error) {
	root, err := canonicalExistingDirectory(hqRoot, "workspace root")
	if err != nil {
		return "", err
	}
	ref := filepath.Clean(workstationReference(rule))
	if err := validatePersonalWorkstationPath(rule.Department, workstationReference(rule), rule.RoleCardVersion); err != nil {
		return "", fmt.Errorf("agent %s：%w", rule.Name, err)
	}
	if ref == "." || filepath.IsAbs(ref) || ref == ".." || strings.HasPrefix(ref, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("agent %s 工位路径必须是 workspace 内相对路径：%s", rule.Name, workstationReference(rule))
	}
	candidate := filepath.Clean(filepath.Join(root, ref))
	if !pathWithin(candidate, root) {
		return "", fmt.Errorf("agent %s 工位越出 workspace：%s", rule.Name, workstationReference(rule))
	}
	canonical, err := canonicalExistingDirectory(candidate, "agent "+rule.Name+" 工位")
	if err != nil {
		return "", fmt.Errorf("agent %s 工位必须是 workspace 内 canonical、非 symlink 目录：%s：%w", rule.Name, candidate, err)
	}
	if canonical != candidate || !pathWithin(canonical, root) {
		return "", fmt.Errorf("agent %s 工位必须是 workspace 内 canonical、非 symlink 目录：%s", rule.Name, candidate)
	}
	return canonical, nil
}

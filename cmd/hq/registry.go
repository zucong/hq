package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxRoleManualBytes int64 = 1 << 20

func readRoleManualFile(hqRoot, path, label string, beforeOpen func(string) error) ([]byte, string, error) {
	roots, err := resolveReferenceRoots(referencePolicy{HQRoot: hqRoot})
	if err != nil {
		return nil, "", err
	}
	file, canonical, err := openAllowedRegularFile(path, roots, beforeOpen)
	if err != nil {
		return nil, "", fmt.Errorf("%s 必须是 workspace 内 canonical、可读、非 symlink 普通文件：%w", label, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxRoleManualBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取 %s：%w", label, err)
	}
	if int64(len(raw)) > maxRoleManualBytes {
		return nil, "", fmt.Errorf("%s 超过最大 %d bytes", label, maxRoleManualBytes)
	}
	return raw, canonical, nil
}

func validateRegistryManuals(cfg Config, hqRoot string) error {
	root, err := canonicalExistingDirectory(hqRoot, "workspace root")
	if err != nil {
		return err
	}
	for _, card := range cfg.RoleCards {
		if _, err := verifyRoleCardArtifact(root, card); err != nil {
			return err
		}
	}
	for _, rule := range cfg.Agents {
		if rule.Disabled {
			continue
		}
		if _, err := verifyAgentRoleCardArtifact(cfg, root, rule); err != nil {
			return err
		}
	}
	return nil
}

func verifyRoleCardArtifact(hqRoot string, card RoleCard) (string, error) {
	if err := validatePersonalManualPath(card.Department, card.ManualPath, card.Version); err != nil {
		return "", fmt.Errorf("role card %s 手册路径不符合个人工位合同：%w", roleCardKey(card.ID, card.Version), err)
	}
	manualRef := filepath.Clean(strings.TrimSpace(card.ManualPath))
	manual := filepath.Clean(filepath.Join(hqRoot, manualRef))
	label := "role card " + roleCardKey(card.ID, card.Version) + " AGENTS.md"
	raw, canonical, err := readRoleManualFile(hqRoot, manual, label, nil)
	if err != nil {
		return "", err
	}
	if canonical != manual {
		return "", fmt.Errorf("role card %s 手册必须使用 canonical 路径：%s", roleCardKey(card.ID, card.Version), manual)
	}
	if actual := roleCardFileDigest(raw); actual != card.ManualDigest {
		return "", fmt.Errorf("role card %s manual digest 漂移：registry=%s actual=%s", roleCardKey(card.ID, card.Version), card.ManualDigest, actual)
	}
	if card.Digest != roleCardDigest(card) {
		return "", fmt.Errorf("role card %s contract digest 漂移", roleCardKey(card.ID, card.Version))
	}
	return canonical, nil
}

func verifyAgentRoleCardArtifact(cfg Config, hqRoot string, rule AgentRule) (string, error) {
	if err := validatePersonalWorkstationPath(rule.Department, rule.WorkstationPath, rule.RoleCardVersion); err != nil {
		return "", fmt.Errorf("员工 %s 工位不符合个人工位合同：%w", rule.Name, err)
	}
	card, err := cfg.roleCardForAgent(rule)
	if err != nil {
		return "", err
	}
	manual, err := verifyRoleCardArtifact(hqRoot, card)
	if err != nil {
		return "", err
	}
	if filepath.Clean(card.ManualPath) != filepath.Clean(rule.ManualPath) {
		return "", fmt.Errorf("员工 %s manual_path 与 role card 不一致", rule.Name)
	}
	if filepath.Clean(rule.ManualPath) != filepath.Join(filepath.Clean(rule.WorkstationPath), "AGENTS.md") {
		return "", fmt.Errorf("员工 %s manual_path 必须正好等于 workstation_path/AGENTS.md", rule.Name)
	}
	return manual, nil
}

func resolveRegistryManual(hqRoot string, rule AgentRule) (string, error) {
	manualRef := filepath.Clean(strings.TrimSpace(rule.ManualPath))
	if manualRef == "." || filepath.IsAbs(manualRef) || manualRef == ".." || strings.HasPrefix(manualRef, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("agent %s 岗位手册路径必须是 workspace 内相对路径：%s", rule.Name, rule.ManualPath)
	}
	departmentRoot := filepath.Clean(filepath.Join(hqRoot, rule.Department))
	manual := filepath.Clean(filepath.Join(hqRoot, manualRef))
	rel, err := filepath.Rel(departmentRoot, manual)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("agent %s 岗位手册越出所属部门 %s：%s", rule.Name, rule.Department, rule.ManualPath)
	}
	canonical, err := canonicalExistingRegularFile(manual, "agent "+rule.Name+" 岗位手册")
	if err != nil || canonical != manual {
		return "", fmt.Errorf("agent %s 岗位手册必须是 canonical、可读、非 symlink 普通文件：%s：%w", rule.Name, manual, err)
	}
	file, err := os.Open(canonical)
	if err != nil {
		return "", fmt.Errorf("agent %s 岗位手册不可读：%s：%w", rule.Name, canonical, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("agent %s 岗位手册读取失败：%s：%w", rule.Name, canonical, err)
	}
	return canonical, nil
}

func (c Config) approvalWitness() (AgentRule, error) {
	rule, ok := c.uniqueRole(roleApprovalWitness)
	if !ok || rule.Disabled || !rule.CanIssue || c.isManager(rule) {
		return AgentRule{}, fmt.Errorf("当前 workspace 缺失、重复或未授权职责位 %s", roleApprovalWitness)
	}
	return rule, nil
}

func (c Config) accountCloser() (AgentRule, error) {
	rule, ok := c.uniqueRole(roleAccountCloser)
	if !ok || rule.Disabled || !rule.CanClose {
		return AgentRule{}, fmt.Errorf("当前 workspace 缺失、重复或未授权职责位 %s", roleAccountCloser)
	}
	return rule, nil
}

// canCloseAsAccount resolves the unique current-workspace account_closer
// responsibility and fails closed.
func (c Config) canCloseAsAccount(rule AgentRule) bool {
	closer, err := c.accountCloser()
	return err == nil && closer.Name == rule.Name
}

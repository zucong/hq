package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	doctorReportVersion = 1
	doctorStatusPass    = "PASS"
	doctorStatusFail    = "FAIL"
	doctorStatusBlocked = "BLOCKED"
	accessWrite         = uint32(2)
	accessExecute       = uint32(1)
)

type DoctorCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
}

type DoctorReport struct {
	Version       int                  `json:"version"`
	OK            bool                 `json:"ok"`
	Checks        []DoctorCheck        `json:"checks"`
	CompanyHealth *CompanyHealthReport `json:"company_health,omitempty"`
}

// DoctorFailedError carries the non-zero exit contract after the full report
// has already been emitted. main deliberately does not add a human prefix.
type DoctorFailedError struct{}

func (DoctorFailedError) Error() string { return "hq doctor 发现硬失败" }

// DoctorRunner isolates the only subprocess used by doctor. Tests inject a
// fake; a nil runner is a hard failure and never falls back to a real command.
type DoctorRunner interface {
	WorkspaceList(herdrBin, workspaceLabel string) error
}

type controlDoctorRunner struct{ Control HerdrControl }

func (r controlDoctorRunner) WorkspaceList(_, workspaceLabel string) error {
	if r.Control == nil {
		return fmt.Errorf("Herdr control 未注入")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.Control.Snapshot(ctx, HerdrSnapshotScope{WorkspaceLabel: workspaceLabel})
	return err
}

func newDoctorApp(options globalOptions, out, errOut io.Writer) (*App, error) {
	office, err := locateOfficeForDoctor(options.Office)
	if err != nil {
		return nil, err
	}
	hqRoot := filepath.Dir(office)
	dataDir := options.Data
	if dataDir == "" {
		dataDir = filepath.Join(office, "records")
	}
	configPath := options.Config
	if configPath == "" {
		configPath = defaultConfigPath(office)
	}
	herdrBin := options.Herdr
	if herdrBin == "" {
		herdrBin = os.Getenv("HQ_HERDR_BIN")
	}
	resolvedHerdr, resolveErr := resolveHerdrExecutable(herdrBin)
	var control HerdrControl
	if resolveErr != nil {
		herdrBin = "/nonexistent/herdr"
		control = unavailableHerdrControl{Err: resolveErr}
	} else {
		herdrBin = resolvedHerdr
		execControl, controlErr := newExecHerdrControl(herdrBin)
		if controlErr != nil {
			control = unavailableHerdrControl{Err: controlErr}
		} else {
			control = execControl
		}
	}
	return &App{
		Office: office, HQRoot: hqRoot, DataDir: dataDir,
		ConfigPath: configPath, HerdrBin: herdrBin, JSON: options.JSON,
		Out: out, Err: errOut, DoctorRunner: controlDoctorRunner{Control: control}, Herdr: control,
		PatrolRunner:  &PatrolService{Herdr: control, Sleep: time.Sleep},
		GatewayHealth: unixGatewayPinger{}, LedgerHealth: readOnlyLedgerHealth{Dir: dataDir},
	}, nil
}

// locateOfficeForDoctor identifies the path without requiring config.yaml to
// exist. The canonical registry is one of doctor's checks and must be diagnosable.
func locateOfficeForDoctor(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("HQ_OFFICE")
	}
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if office := existingOfficeCandidate(cwd); office != "" {
		return office, nil
	}
	if executable, execErr := os.Executable(); execErr == nil {
		if office := existingOfficeCandidate(filepath.Dir(executable)); office != "" {
			return office, nil
		}
	}
	return "", fmt.Errorf("无法定位 ceo-office；请传 --office 或设置 HQ_OFFICE 后重试 hq doctor")
}

func existingOfficeCandidate(start string) string {
	for dir := start; ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "ceo-office" {
			return dir
		}
		candidate := filepath.Join(dir, "ceo-office")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}

func (a *App) cmdDoctor(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("用法：hq doctor [--json]")
	}
	report := a.doctorReport()
	if err := a.writeDoctorReport(report); err != nil {
		return err
	}
	if !report.OK {
		return DoctorFailedError{}
	}
	return nil
}

func (a *App) doctorReport() DoctorReport {
	report := DoctorReport{Version: doctorReportVersion, OK: true}
	add := func(check DoctorCheck) {
		report.Checks = append(report.Checks, check)
		if check.Status == doctorStatusFail || check.Status == doctorStatusBlocked {
			report.OK = false
		}
	}

	info, officeErr := os.Lstat(a.Office)
	if officeErr == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir() {
		add(passDoctorCheck("office_directory", "CEO 办公室目录存在且不是 symlink", ""))
	} else {
		add(failDoctorCheck("office_directory", "CEO 办公室目录缺失、不是目录或是 symlink", "确认 --office 指向实际的非 symlink ceo-office 目录，再重试 hq doctor"))
	}

	cfg, configErr := loadConfig(a.ConfigPath)
	if configErr != nil {
		examplePath := filepath.Join(a.Office, "tools", "hq", "config.example.yaml")
		add(configFailureDoctorCheck(a.ConfigPath, examplePath, configErr))
	} else {
		a.Config = cfg
		add(passDoctorCheck("config", "HQ config 存在且合法", ""))
	}

	add(a.checkDecisions())
	if configErr != nil {
		add(blockedDoctorCheck("agent_workstations", "config 不可用，无法安全枚举在职 agent", "先按 config 项给出的安全步骤恢复配置，再重试 hq doctor"))
	} else {
		rules := append([]AgentRule(nil), cfg.Agents...)
		sort.Slice(rules, func(i, j int) bool { return rules[i].Name < rules[j].Name })
		for _, rule := range rules {
			if rule.Disabled {
				continue
			}
			add(a.checkAgentWorkstation(rule))
		}
	}

	if a.DoctorRunner == nil {
		add(failDoctorCheck("herdr_workspace", "herdr workspace list runner 未配置，已拒绝执行", "为 doctor 注入受控 runner；测试必须使用 fake/shim"))
	} else if configErr != nil {
		add(blockedDoctorCheck("herdr_workspace", "config 不可用，无法确定 HQ 目标 workspace", "先恢复 config，再重试 hq doctor"))
	} else if err := a.DoctorRunner.WorkspaceList(a.HerdrBin, cfg.WorkspaceLabel); err != nil {
		add(failDoctorCheck("herdr_workspace", "herdr workspace list 执行失败", "确认 herdr 已安装、可执行且当前会话可运行 herdr workspace list"))
	} else {
		add(passDoctorCheck("herdr_workspace", "herdr workspace list 可执行", ""))
	}

	if configErr != nil {
		add(blockedDoctorCheck("company_health", "config 不可用，无法安全汇总公司健康", "先恢复 config，再重试 hq doctor"))
	} else if a.PatrolRunner == nil || a.GatewayHealth == nil || a.LedgerHealth == nil {
		add(blockedDoctorCheck("company_health", "公司健康只读依赖未完整注入", "为 doctor 注入 patrol、gateway pinger 与只读 ledger reader"))
	} else {
		health := CompanyHealthReport{}
		patrol, patrolErr := a.PatrolRunner.Run(context.Background(), cfg, a.HQRoot, 100*time.Millisecond)
		health.Patrol = patrol
		if patrolErr != nil {
			health.Errors = append(health.Errors, "patrol: "+patrolErr.Error())
		}
		socket, socketErr := gatewaySocketPath(a.DataDir)
		if socketErr != nil {
			health.Gateway = GatewayHealth{Error: "解析 gateway socket：" + socketErr.Error()}
		} else if gatewayExpectedNotStarted(a.DataDir, socket) {
			health.Gateway = GatewayHealth{NotStarted: true}
		} else {
			health.Gateway = a.GatewayHealth.Ping(context.Background(), socket, patrol.WorkspaceID)
		}
		gatewayFailed := !health.Gateway.OK && !health.Gateway.NotStarted
		if gatewayFailed {
			health.Errors = append(health.Errors, "gateway: "+health.Gateway.Error)
		}
		ledger, ledgerErr := a.LedgerHealth.Read(cfg)
		health.Ledger = ledger
		if ledgerErr != nil {
			health.Errors = append(health.Errors, "ledger: "+ledgerErr.Error())
		}
		report.CompanyHealth = &health
		if patrolErr != nil || ledgerErr != nil || gatewayFailed || patrol.Drift != 0 || patrol.Orphan != 0 || patrol.DeadCandidate != 0 {
			add(failDoctorCheck("company_health", health.message(), "按 company_health 结构化详情修复编制漂移、网关协议或坏账本；doctor 不会自动处置"))
		} else {
			add(passDoctorCheck("company_health", health.message(), "只读汇总；blocked 仅报告，不单独判死"))
		}
	}

	binary := filepath.Join(a.Office, "tools", "hq", "bin", "hq")
	if regularFile(binary, true) {
		add(passDoctorCheck("binary", "bin/hq 已构建且可执行", ""))
	} else {
		add(failDoctorCheck("binary", "bin/hq 缺失、不是普通文件或不可执行", "在 "+filepath.Dir(filepath.Dir(binary))+" 运行 go build -trimpath -o bin/hq ."))
	}

	add(checkRecords(a.DataDir))
	return report
}

func gatewayExpectedNotStarted(dataDir, socket string) bool {
	if _, err := os.Lstat(dataDir); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_, err := os.Lstat(socket)
	return errors.Is(err, os.ErrNotExist)
}

func configFailureDoctorCheck(configPath, examplePath string, configErr error) DoctorCheck {
	quotedConfig := shellQuote(configPath)
	quotedExample := shellQuote(examplePath)

	if errors.Is(configErr, os.ErrNotExist) {
		return failDoctorCheck(
			"config",
			"HQ config 缺失",
			"从本公司已批准的备份或版本控制恢复 "+quotedConfig+"；若尚未创建公司，按 README 运行 hq init。"+quotedExample+" 只说明字段结构，其 digest 不属于本公司，不能直接作为运行配置",
		)
	}
	if errors.Is(configErr, os.ErrPermission) {
		return failDoctorCheck(
			"config",
			"HQ config 存在但当前用户不可读",
			"运行 ls -l "+quotedConfig+" 检查读取权限；修正权限后重试 hq doctor，doctor 不会覆盖该文件",
		)
	}

	message := configErr.Error()
	if strings.Contains(message, "严格 YAML 解码失败") || strings.Contains(message, "不得使用 JSON 文档") || strings.Contains(message, "YAML 注册表为空") || strings.Contains(message, "YAML 尾部") {
		return failDoctorCheck(
			"config",
			"HQ config YAML 解析失败",
			"对照 "+quotedExample+" 检查 YAML 缩进、重复键与未知字段；修正原文件后重试 hq doctor，doctor 不会覆盖该文件",
		)
	}

	backupPath := configPath + ".bak"
	return failDoctorCheck(
		"config",
		"HQ config 已解析，但未通过语义校验",
		"先做非覆盖备份：cp -np "+quotedConfig+" "+shellQuote(backupPath)+"；再对照 "+quotedExample+" 与 README 的字段清单修正原文件并重试 hq doctor",
	)
}

func (a *App) checkDecisions() DoctorCheck {
	directory := filepath.Join(a.Office, "decisions")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return failDoctorCheck("decisions", "decisions 目录不存在或不可读", "创建 "+directory+"，并由 "+a.Config.ownerPrincipal()+" 确认至少一份生效决策")
	}
	valid := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if _, err := validateApproval(filepath.Join(directory, entry.Name()), a.Office, a.HQRoot, a.Config.ownerPrincipal()); err == nil {
			valid++
		}
	}
	if valid == 0 {
		return failDoctorCheck("decisions", "decisions 目录内没有符合现有批准合同的生效决策", "由 "+a.Config.ownerPrincipal()+" 确认至少一份 decisions 内决策文件；doctor 不会代为创建或批准")
	}
	return passDoctorCheck("decisions", fmt.Sprintf("decisions 目录可用，发现 %d 份符合现有合同的生效决策", valid), "")
}

func (a *App) checkAgentWorkstation(rule AgentRule) DoctorCheck {
	name := "agent_workstation/" + rule.Name
	directory, err := resolveAgentWorkstation(a.HQRoot, rule)
	if err != nil {
		return failDoctorCheck(name, "在职 agent "+rule.Name+" 的登记工位不可用", err.Error())
	}
	if rule.ManualPath == "" {
		manual := filepath.Join(directory, "AGENTS.md")
		if !regularFile(manual, false) {
			return failDoctorCheck(name, "在职 agent "+rule.Name+" 的工位 AGENTS.md 缺失或不是普通文件", "补齐并确认 "+manual)
		}
		return passDoctorCheck(name, "在职 agent "+rule.Name+" 的独立工位与 AGENTS.md 可用", "")
	}
	manual, manualErr := resolveRegistryManual(a.HQRoot, rule)
	if manualErr != nil {
		return failDoctorCheck(name, "在职 agent "+rule.Name+" 的岗位手册不可用", manualErr.Error())
	}
	return passDoctorCheck(name, "在职 agent "+rule.Name+" 的独立工位与岗位手册可用："+manual, "")
}

func checkRecords(path string) DoctorCheck {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return failDoctorCheck("records", "records 路径存在但不是目录", "将 "+path+" 修复为可写目录；doctor 不会 rename 或删除现有路径")
		}
		if !directoryWritable(path, info) {
			return failDoctorCheck("records", "records 目录按当前权限与只读元数据判断不可写", "为当前用户授予 "+path+" 的写入与进入权限；doctor 不会 chmod")
		}
		return passDoctorCheck("records", "records 目录存在且按当前权限与只读元数据判断可写", "")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return failDoctorCheck("records", "records 路径不可检查", "确认 "+path+" 及其父目录可访问")
	}
	parent, parentInfo, parentErr := nearestExistingDirectory(filepath.Dir(path))
	if parentErr != nil || !directoryWritable(parent, parentInfo) {
		return failDoctorCheck("records", "records 尚不存在，且最近现有父目录不可写，首次写入无法自愈", "为 "+filepath.Dir(path)+" 的现有父目录授予当前用户写入与进入权限")
	}
	return DoctorCheck{
		Name: "records", Status: doctorStatusPass, Severity: "advisory",
		Message:     "records 不存在；缺失本身不判失败，doctor 未创建目录",
		Remediation: "无需预创建；HQ 首次写入会自愈创建 records（当前父目录允许合法写入）",
	}
}

func nearestExistingDirectory(path string) (string, os.FileInfo, error) {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return current, info, fmt.Errorf("最近现有父路径不是目录")
			}
			return current, info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return current, nil, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil, fmt.Errorf("找不到现有父目录")
		}
	}
}

func directoryWritable(path string, info os.FileInfo) bool {
	if info == nil || !info.IsDir() {
		return false
	}
	permissions := info.Mode().Perm()
	if permissions&0o222 == 0 || permissions&0o111 == 0 {
		return false
	}
	return syscall.Access(path, accessWrite|accessExecute) == nil
}

func regularFile(path string, executable bool) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return !executable || info.Mode().Perm()&0o111 != 0
}

func passDoctorCheck(name, message, remediation string) DoctorCheck {
	return DoctorCheck{Name: name, Status: doctorStatusPass, Severity: "info", Message: message, Remediation: remediation}
}

func failDoctorCheck(name, message, remediation string) DoctorCheck {
	return DoctorCheck{Name: name, Status: doctorStatusFail, Severity: "hard", Message: message, Remediation: remediation}
}

func blockedDoctorCheck(name, message, remediation string) DoctorCheck {
	return DoctorCheck{Name: name, Status: doctorStatusBlocked, Severity: "hard", Message: message, Remediation: remediation}
}

func (a *App) writeDoctorReport(report DoctorReport) error {
	if a.JSON {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(a.Out, "%-7s %-30s %s\n", check.Status, check.Name, check.Message); err != nil {
			return err
		}
		if strings.TrimSpace(check.Remediation) != "" {
			prefix := "提示："
			if check.Status == doctorStatusFail || check.Status == doctorStatusBlocked {
				prefix = "修复："
			}
			if _, err := fmt.Fprintf(a.Out, "        %s%s\n", prefix, check.Remediation); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(a.Out, "HQ doctor：ok=%t checks=%d\n", report.OK, len(report.Checks))
	return err
}

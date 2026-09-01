package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

const (
	gatewayProtocolVersion = 1
	gatewayRequestLimit    = 1 << 20
)

type gatewayRequest struct {
	Version   int      `json:"version"`
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	PaneID    string   `json:"pane_id"`
	Workspace string   `json:"workspace_id,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
	JSON      bool     `json:"json,omitempty"`
}

type gatewayResponse struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	Workspace string `json:"workspace_id"`
	ServerID  string `json:"server_id"`
	OK        bool   `json:"ok"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Error     string `json:"error,omitempty"`
}

func shouldUseGateway(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return false
		}
	}
	switch args[0] {
	case "whoami", "report", "issue", "approval", "message", "inbox", "accept", "return", "close", "delivery", "nudge", "reminder":
		return true
	case "case":
		return len(args) > 1 && (args[1] == "create" || args[1] == "escalate" || args[1] == "revise")
	case "staff":
		return len(args) > 1 && (args[1] == "add" || args[1] == "update" || args[1] == "remove")
	case "role":
		return len(args) > 1 && (args[1] == "add" || args[1] == "retire")
	default:
		return false
	}
}

func forwardToGateway(socket string, args []string, dryRun, jsonOutput bool, out, errOut io.Writer) error {
	return forwardToGatewayWithTimeouts(socket, args, dryRun, jsonOutput, out, errOut, defaultGatewayTimeoutPolicy())
}

func forwardToGatewayWithTimeouts(socket string, args []string, dryRun, jsonOutput bool, out, errOut io.Writer, policy gatewayTimeoutPolicy) error {
	if !policy.valid() {
		return fmt.Errorf("gateway timeout policy 要求 business > health > 0")
	}
	paneID := os.Getenv("HERDR_PANE_ID")
	workspaceID := os.Getenv("HERDR_WORKSPACE_ID")
	if os.Getenv("HERDR_ENV") != "1" || paneID == "" || workspaceID == "" {
		return fmt.Errorf("网关写操作必须从带 pane/workspace 身份的 herdr agent 工位调用")
	}
	request := gatewayRequest{
		Version: gatewayProtocolVersion, Type: "request", Args: args, PaneID: paneID,
		Workspace: workspaceID, DryRun: dryRun, JSON: jsonOutput,
	}
	response, connected, err := exchangeGatewayWithTimeouts(context.Background(), socket, request, policy)
	if err != nil {
		if connected {
			return gatewayAmbiguousResult(socket, args, err)
		}
		return fmt.Errorf("连接 HQ 网关 %s：%w", socket, err)
	}
	if response.Version != gatewayProtocolVersion || response.Type != "response" || response.Workspace != workspaceID || response.ServerID == "" {
		return gatewayAmbiguousResult(socket, args, fmt.Errorf("协议版本、workspace 或 server identity 不匹配：version=%d type=%q workspace=%q server=%q expected_workspace=%q", response.Version, response.Type, response.Workspace, response.ServerID, workspaceID))
	}
	if response.Stdout != "" {
		fmt.Fprint(out, response.Stdout)
	}
	if response.Stderr != "" {
		fmt.Fprint(errOut, response.Stderr)
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "HQ 网关拒绝请求"
		}
		return errors.New(response.Error)
	}
	return nil
}

func gatewayAmbiguousResult(socket string, args []string, cause error) error {
	return conflictf("HQ 网关 %s 连接已建立，但未收到可验证的业务终态：%v；结果不确定，命令可能已经落账或投递，禁止直接重复执行。先运行只读核验：%s；如发现 pending/unknown delivery，再按其 next_action 恢复", socket, cause, gatewayReadOnlyRecovery(args))
}

func gatewayReadOnlyRecovery(args []string) string {
	caseID := gatewayFlagValue(args, "case")
	if caseID != "" && validateCaseID(caseID) == nil {
		if len(args) > 0 && args[0] == "issue" {
			return fmt.Sprintf("hq history --case %s；hq assignment list --case %s", caseID, caseID)
		}
		return fmt.Sprintf("hq history --case %s；hq case show --id %s", caseID, caseID)
	}
	return "hq board --cases-only；hq assignment list"
}

func gatewayFlagValue(args []string, name string) string {
	flag := "--" + name
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return strings.TrimSpace(args[index+1])
		}
		if strings.HasPrefix(arg, flag+"=") {
			return strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
		}
	}
	return ""
}

func (a *App) cmdServe(args []string) error {
	return a.serveGatewayCommand(args)
}

func (a *App) handleGatewayConn(conn net.Conn) {
	a.handleGatewayConnection(conn, a.GatewayWorkspaceID, a.GatewayServerID)
}

func (a *App) cmdPing(args []string) error {
	fs := newLeafParser("ping")
	fs.SetOutput(a.Err)
	workspace := fs.String("workspace", "", "预期 herdr workspace id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法：hq ping --workspace ID")
	}
	if *workspace == "" {
		if a.GatewayWorkspaceID != "" {
			*workspace = a.GatewayWorkspaceID
		} else if a.Herdr != nil {
			snapshot, err := a.herdrSnapshot(context.Background())
			if err != nil {
				return fmt.Errorf("自动解析公司 workspace：%w", err)
			}
			for _, candidate := range snapshot.Workspaces {
				if candidate.Label == a.Config.WorkspaceLabel {
					if *workspace != "" {
						return fmt.Errorf("workspace label %s 匹配多个稳定 ID；请显式使用 --workspace", a.Config.WorkspaceLabel)
					}
					*workspace = candidate.ID
				}
			}
		} else {
			*workspace = os.Getenv("HERDR_WORKSPACE_ID")
		}
	}
	if *workspace == "" {
		return fmt.Errorf("无法解析公司 workspace id；请显式使用 --workspace ID")
	}
	socket, err := gatewaySocketPath(a.DataDir)
	if err != nil {
		return err
	}
	pinger := a.GatewayHealth
	if pinger == nil {
		pinger = unixGatewayPinger{}
	}
	health := pinger.Ping(context.Background(), socket, *workspace)
	if !health.OK {
		return fmt.Errorf("HQ 网关未通过协议身份核验：%s", health.Error)
	}
	_, err = fmt.Fprintf(a.Out, "HQ 网关在线：%s workspace=%s server=%s\n", socket, health.Workspace, health.ServerID)
	return err
}

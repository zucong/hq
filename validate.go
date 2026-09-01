package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	caseIDPattern  = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{2,63}$`)
	moneyPattern   = regexp.MustCompile(`(?:[¥￥$]\s*[0-9]|[0-9][0-9,.]*\s*(?:元|万元|美元|人民币))`)
	secretPatterns = []string{".env", "apikey", "api_key", "token=", "cookie=", "password=", "secret="}
)

const maxBusinessTextBytes = 2 * 1024

func newEventID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	stamp := time.Now().Format("20060102T150405.000000000")
	return "EVT-" + stamp + "-" + strings.ToUpper(hex.EncodeToString(buf)), nil
}

func validateCaseID(id string) error {
	if !caseIDPattern.MatchString(id) {
		return fmt.Errorf("case_id 必须匹配 %s", caseIDPattern.String())
	}
	return nil
}

func validateShortText(name, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("缺少 --%s", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("--%s 必须是单行文本", name)
	}
	if utf8.RuneCountInString(value) > 200 {
		return "", fmt.Errorf("--%s 超过 200 个字符，应把长内容写入权威原文后只传路径", name)
	}
	if containsSensitive(value) {
		return "", fmt.Errorf("--%s 疑似包含密钥或金额，拒绝写入事件账本", name)
	}
	return value, nil
}

// validateBusinessText is the common contract for human-authored business
// narrative carried in the ledger and delivery envelopes. Structural IDs,
// labels, refs, and control-plane reminders intentionally retain their
// narrower validators.
func validateBusinessText(name, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("缺少 --%s", name)
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("--%s 必须是不含 CR/LF/NUL 的合法 UTF-8 单行文本", name)
	}
	if len([]byte(value)) > maxBusinessTextBytes {
		return "", fmt.Errorf("--%s 为 %d bytes，超过 2 KiB 硬上限；请把更长内容写入权威原文后只传路径", name, len([]byte(value)))
	}
	if containsSensitive(value) {
		return "", fmt.Errorf("--%s 疑似包含密钥或金额，拒绝写入事件账本", name)
	}
	return value, nil
}

func validateEventShortFields(event Event) error {
	fields := []struct {
		name  string
		value string
	}{
		{"title", event.Title}, {"project", event.Project}, {"result", event.Result},
		{"severity", event.Severity}, {"location", event.Location}, {"dedupe_key", event.DedupeKey},
		{"claim_id", event.ClaimID}, {"reminder_id", event.ReminderID},
		{"basis_event_id", event.BasisEventID}, {"estop_id", event.EstopID},
		{"workspace_id", event.WorkspaceID}, {"tab_id", event.TabID}, {"pane_id", event.PaneID},
		{"agent_kind", event.AgentKind}, {"cwd", event.CWD},
		{"approval_id", event.ApprovalID}, {"approval_action", event.ApprovalAction},
		{"approval_status", event.ApprovalStatus}, {"approval_mode", event.ApprovalMode},
		{"authorization_type", event.AuthorizationType}, {"issuer", event.Issuer},
		{"captured_by", event.CapturedBy},
		{"priority", event.Priority}, {"message_kind", event.MessageKind}, {"message_id", event.MessageID},
		{"thread_id", event.ThreadID}, {"reply_to", event.ReplyTo},
	}
	for _, field := range fields {
		if strings.ContainsAny(field.value, "\r\n") {
			return fmt.Errorf("事件字段 %s 必须是单行文本", field.name)
		}
		if utf8.RuneCountInString(field.value) > 200 {
			return fmt.Errorf("事件字段 %s 超过 200 个 Unicode rune", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"verification", event.Verification}, {"next_action", event.NextAction}, {"note", event.Note},
	} {
		if !utf8.ValidString(field.value) || strings.ContainsAny(field.value, "\r\n\x00") {
			return fmt.Errorf("事件业务字段 %s 必须是合法 UTF-8 单行文本", field.name)
		}
		if len([]byte(field.value)) > maxBusinessTextBytes {
			return fmt.Errorf("事件业务字段 %s 超过 2 KiB", field.name)
		}
	}
	if event.Message != "" {
		if !utf8.ValidString(event.Message) || len([]byte(event.Message)) > maxMessageTextBytes {
			return fmt.Errorf("事件字段 message 超过 2 KiB 或不是合法 UTF-8")
		}
		if strings.ContainsAny(event.Message, "\r\x00") {
			return fmt.Errorf("事件字段 message 含 CR 或 NUL")
		}
	}
	for _, refs := range [][]string{event.RefFiles, event.RefCases, event.RefMessages, event.RefEvents} {
		for _, ref := range refs {
			if strings.ContainsAny(ref, "\r\n") || utf8.RuneCountInString(ref) > 200 {
				return fmt.Errorf("事件结构化引用必须是至多 200 rune 的单行文本")
			}
		}
	}
	return nil
}

func containsSensitive(value string) bool {
	lower := strings.ToLower(value)
	for _, pattern := range secretPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return moneyPattern.MatchString(value)
}

func normalizeRef(value, hqRoot string, required bool) (string, error) {
	return normalizeReference(value, hqRoot, required)
}

func validateApproval(value, office, hqRoot, ownerPrincipal string) (string, error) {
	_ = hqRoot
	return validateApprovalReference(value, office, ownerPrincipal)
}

func validateSeverity(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	switch value {
	case "P0", "P1", "P2":
		return value, nil
	default:
		return "", fmt.Errorf("severity 只能是 P0/P1/P2")
	}
}

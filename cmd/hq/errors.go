package main

import (
	"errors"
	"fmt"
	"strings"
)

// Exit codes follow the portable sysexits meanings. They are part of the CLI
// contract and intentionally do not depend on translated error text.
const (
	exitSuccess     = 0
	exitUsage       = 64
	exitUnavailable = 69
	exitInternal    = 70
	exitConflict    = 75
	exitPermission  = 77
)

type errorCategory string

const (
	categoryUsage       errorCategory = "usage"
	categoryUnavailable errorCategory = "temporarily_unavailable"
	categoryInternal    errorCategory = "internal"
	categoryConflict    errorCategory = "conflict"
	categoryPermission  errorCategory = "permission"
)

type categorizedError struct {
	Category errorCategory
	Cause    error
}

// agentCommandError is the public CLI repair contract. Every failed public
// command keeps its original typed cause (and therefore its exit category),
// while also telling an agent which command failed, how it is shaped, and how
// to discover the complete local contract without an external manual.
type agentCommandError struct {
	Command string
	Usage   string
	Help    string
	Cause   error
}

func (e *agentCommandError) Error() string {
	cause := e.Cause.Error()
	message := fmt.Sprintf("命令 `%s` 未完成：%s", e.Command, cause)
	if e.Usage != "" && !strings.Contains(cause, "用法：") {
		message += "\n用法：" + e.Usage
	}
	message += fmt.Sprintf("\n下一步：运行 `%s` 查看该命令的完整参数、必填项与约束；按上方原因修正后重试同一 HQ 命令。若错误表明 event/delivery 已记账，先运行 `hq delivery status --id DELIVERY_ID` 或 `hq flow show --case CASE_ID` 核验状态并复用原 ID，不要重发业务命令，也不要改用裸 herdr prompt。", e.Help)
	return message
}

func (e *agentCommandError) Unwrap() error { return e.Cause }

func (e *categorizedError) Error() string { return e.Cause.Error() }
func (e *categorizedError) Unwrap() error { return e.Cause }

func categoryf(category errorCategory, format string, args ...any) error {
	return &categorizedError{Category: category, Cause: fmt.Errorf(format, args...)}
}

func usagef(format string, args ...any) error {
	return categoryf(categoryUsage, format, args...)
}

func permissionf(format string, args ...any) error {
	return categoryf(categoryPermission, format, args...)
}

func conflictf(format string, args ...any) error {
	return categoryf(categoryConflict, format, args...)
}

func unavailablef(format string, args ...any) error {
	return categoryf(categoryUnavailable, format, args...)
}

func internalf(format string, args ...any) error {
	return categoryf(categoryInternal, format, args...)
}

func exitCodeForError(err error) int {
	if err == nil {
		return exitSuccess
	}
	var doctorFailed DoctorFailedError
	if errors.As(err, &doctorFailed) {
		// Preserve doctor's long-standing health-check contract: report first,
		// then exit 1 without adding another user-facing error line.
		return 1
	}
	var categorized *categorizedError
	if !errors.As(err, &categorized) {
		return exitInternal
	}
	switch categorized.Category {
	case categoryUsage:
		return exitUsage
	case categoryUnavailable:
		return exitUnavailable
	case categoryConflict:
		return exitConflict
	case categoryPermission:
		return exitPermission
	default:
		return exitInternal
	}
}

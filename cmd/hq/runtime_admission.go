package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type RuntimeAdmissionAction string

const (
	runtimeAdmissionAgentStart     RuntimeAdmissionAction = "agent_start"
	runtimeAdmissionAgentResume    RuntimeAdmissionAction = "agent_resume"
	runtimeAdmissionAgentPrompt    RuntimeAdmissionAction = "agent_prompt"
	runtimeAdmissionAgentHibernate RuntimeAdmissionAction = "agent_hibernate"
	runtimeAdmissionControlPlane   RuntimeAdmissionAction = "control_plane"
)

type RuntimeAdmissionCode string

const (
	runtimeAdmissionAllowed          RuntimeAdmissionCode = "runtime_admission_allowed"
	runtimeAdmissionEstopActive      RuntimeAdmissionCode = "runtime_admission_estop_active"
	runtimeAdmissionStateUnavailable RuntimeAdmissionCode = "runtime_admission_state_unavailable"
	runtimeAdmissionInvalidRequest   RuntimeAdmissionCode = "runtime_admission_invalid_request"
)

type RuntimeAdmissionRequest struct {
	Action RuntimeAdmissionAction `json:"action"`
	Target string                 `json:"target"`
}

type RuntimeAdmissionDecision struct {
	Allowed bool                   `json:"allowed"`
	Code    RuntimeAdmissionCode   `json:"code"`
	Action  RuntimeAdmissionAction `json:"action"`
	Target  string                 `json:"target"`
	EstopID string                 `json:"estop_id,omitempty"`
	Reason  string                 `json:"reason"`
}

type RuntimeAdmissionError struct {
	Decision RuntimeAdmissionDecision
	Cause    error
}

func (e *RuntimeAdmissionError) Error() string {
	if e == nil {
		return "runtime admission error"
	}
	message := fmt.Sprintf("%s action=%s target=%s", e.Decision.Code, e.Decision.Action, e.Decision.Target)
	if e.Decision.EstopID != "" {
		message += " estop=" + e.Decision.EstopID
	}
	if e.Decision.Reason != "" {
		message += " reason=" + e.Decision.Reason
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *RuntimeAdmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func validRuntimeAdmissionAction(action RuntimeAdmissionAction) bool {
	switch action {
	case runtimeAdmissionAgentStart, runtimeAdmissionAgentResume, runtimeAdmissionAgentPrompt, runtimeAdmissionAgentHibernate, runtimeAdmissionControlPlane:
		return true
	default:
		return false
	}
}

func invalidRuntimeAdmissionDecision(request RuntimeAdmissionRequest, reason string) RuntimeAdmissionDecision {
	return RuntimeAdmissionDecision{
		Code: runtimeAdmissionInvalidRequest, Action: request.Action, Target: request.Target, Reason: reason,
	}
}

func unavailableRuntimeAdmissionDecision(request RuntimeAdmissionRequest, reason string) RuntimeAdmissionDecision {
	return RuntimeAdmissionDecision{
		Code: runtimeAdmissionStateUnavailable, Action: request.Action, Target: request.Target, Reason: reason,
	}
}

func decideRuntimeAdmission(cfg Config, state EstopState, exists bool, request RuntimeAdmissionRequest) RuntimeAdmissionDecision {
	request.Target = strings.TrimSpace(request.Target)
	if !validRuntimeAdmissionAction(request.Action) {
		return invalidRuntimeAdmissionDecision(request, "unknown-action")
	}
	if request.Target == "" {
		return invalidRuntimeAdmissionDecision(request, "missing-target")
	}
	allowed := RuntimeAdmissionDecision{
		Allowed: true, Code: runtimeAdmissionAllowed, Action: request.Action, Target: request.Target, Reason: "no-active-estop",
	}
	if !exists || state.State == "released" {
		return allowed
	}
	if state.State != "active" {
		return unavailableRuntimeAdmissionDecision(request, "invalid-estop-state")
	}
	if request.Action == runtimeAdmissionControlPlane {
		return RuntimeAdmissionDecision{
			Code: runtimeAdmissionEstopActive, Action: request.Action, Target: request.Target,
			EstopID: state.EstopID, Reason: "control-plane-paused",
		}
	}
	for _, item := range state.Items {
		if item.Agent == request.Target {
			return RuntimeAdmissionDecision{
				Code: runtimeAdmissionEstopActive, Action: request.Action, Target: request.Target,
				EstopID: state.EstopID, Reason: "target-in-frozen-set",
			}
		}
	}
	rule, ok := cfg.exactRule(request.Target)
	if ok && (cfg.isManager(rule) || rule.hasResponsibility(roleAccountCloser)) {
		allowed.EstopID = state.EstopID
		allowed.Reason = "estop-exempt-control-role"
		return allowed
	}
	reason := "target-not-exempt"
	if !ok {
		reason = "target-not-active-in-registry"
	}
	return RuntimeAdmissionDecision{
		Code: runtimeAdmissionEstopActive, Action: request.Action, Target: request.Target,
		EstopID: state.EstopID, Reason: reason,
	}
}

func (s *FileEstopStore) WithRuntimeAdmissions(cfg Config, requests []RuntimeAdmissionRequest, admitted func() error) ([]RuntimeAdmissionDecision, error) {
	return s.WithRuntimeAdmissionsContext(context.Background(), cfg, requests, admitted)
}

func (s *FileEstopStore) WithRuntimeAdmissionsContext(ctx context.Context, cfg Config, requests []RuntimeAdmissionRequest, admitted func() error) ([]RuntimeAdmissionDecision, error) {
	ctx = nonNilContext(ctx)
	if len(requests) == 0 {
		decision := invalidRuntimeAdmissionDecision(RuntimeAdmissionRequest{}, "empty-request-set")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision}
	}
	for _, request := range requests {
		decision := decideRuntimeAdmission(cfg, EstopState{}, false, request)
		if decision.Code == runtimeAdmissionInvalidRequest {
			return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision}
		}
	}
	if s == nil || s.Root == "" {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-store-not-configured")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision}
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-store-create-failed")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	if err := validateOwnedMode(s.Root, 0o700, true); err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-store-invalid")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	lockPath := filepath.Join(s.Root, ".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-lock-open-failed")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	defer lock.Close()
	if err := validateOwnedMode(lockPath, 0o600, false); err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-lock-invalid")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	if err := flockContext(ctx, int(lock.Fd()), syscall.LOCK_SH); err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-lock-failed")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if _, err := os.Lstat(s.tempPath()); err == nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-temp-recovery-required")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision}
	} else if !os.IsNotExist(err) {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-temp-check-failed")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	state, exists, err := s.readLocked()
	if err != nil {
		decision := unavailableRuntimeAdmissionDecision(requests[0], "estop-state-read-failed")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision, Cause: err}
	}
	decisions := make([]RuntimeAdmissionDecision, 0, len(requests))
	for _, request := range requests {
		decision := decideRuntimeAdmission(cfg, state, exists, request)
		decisions = append(decisions, decision)
		if !decision.Allowed {
			return decisions, &RuntimeAdmissionError{Decision: decision}
		}
	}
	if admitted == nil {
		return decisions, nil
	}
	return decisions, admitted()
}

func (a *App) withRuntimeAdmissions(requests []RuntimeAdmissionRequest, admitted func() error) ([]RuntimeAdmissionDecision, error) {
	if a == nil {
		decision := unavailableRuntimeAdmissionDecision(RuntimeAdmissionRequest{}, "app-not-configured")
		return []RuntimeAdmissionDecision{decision}, &RuntimeAdmissionError{Decision: decision}
	}
	return a.Estop.WithRuntimeAdmissionsContext(a.requestContext(), a.Config, requests, admitted)
}

func (a *App) withRuntimeAdmission(request RuntimeAdmissionRequest, admitted func() error) (RuntimeAdmissionDecision, error) {
	decisions, err := a.withRuntimeAdmissions([]RuntimeAdmissionRequest{request}, admitted)
	if len(decisions) == 0 {
		return RuntimeAdmissionDecision{}, err
	}
	return decisions[len(decisions)-1], err
}

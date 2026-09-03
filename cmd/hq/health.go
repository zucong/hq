package main

import (
	"context"
	"fmt"
	"sort"
)

type GatewayHealth struct {
	OK         bool   `json:"ok"`
	Connected  bool   `json:"connected"`
	NotStarted bool   `json:"not_started,omitempty"`
	Version    int    `json:"version,omitempty"`
	Workspace  string `json:"workspace_id,omitempty"`
	ServerID   string `json:"server_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

type GatewayPinger interface {
	Ping(context.Context, string, string) GatewayHealth
}

type LedgerHealthSummary struct {
	Total        int            `json:"total"`
	Open         int            `json:"open"`
	StatusCounts map[string]int `json:"status_counts"`
}

type LedgerHealthReader interface {
	Read(Config) (LedgerHealthSummary, error)
}

type readOnlyLedgerHealth struct {
	Dir string
}

func (r readOnlyLedgerHealth) Read(cfg Config) (LedgerHealthSummary, error) {
	store := &Store{Dir: r.Dir}
	_, ledger, err := store.readLedgerUnlocked(cfg, "", 0)
	if err != nil {
		return LedgerHealthSummary{}, err
	}
	summary := LedgerHealthSummary{StatusCounts: map[string]int{}}
	for _, state := range ledger.snapshot.Cases {
		summary.Total++
		summary.StatusCounts[state.Status]++
		if state.Status != "closed" {
			summary.Open++
		}
	}
	return summary, nil
}

type CompanyHealthReport struct {
	Patrol  PatrolReport        `json:"patrol"`
	Gateway GatewayHealth       `json:"gateway"`
	Ledger  LedgerHealthSummary `json:"ledger"`
	Errors  []string            `json:"errors,omitempty"`
}

func (h CompanyHealthReport) message() string {
	statuses := make([]string, 0, len(h.Ledger.StatusCounts))
	for status, count := range h.Ledger.StatusCounts {
		statuses = append(statuses, fmt.Sprintf("%s=%d", status, count))
	}
	sort.Strings(statuses)
	statusText := "none"
	if len(statuses) != 0 {
		statusText = fmt.Sprintf("%v", statuses)
	}
	gateway := fmt.Sprintf("%t", h.Gateway.OK)
	if h.Gateway.NotStarted {
		gateway = "not-started"
	}
	return fmt.Sprintf("patrol blocked=%d drift=%d orphan=%d dead_candidate=%d；gateway=%s；cases total=%d open=%d status=%s",
		h.Patrol.Blocked, h.Patrol.Drift, h.Patrol.Orphan, h.Patrol.DeadCandidate,
		gateway, h.Ledger.Total, h.Ledger.Open, statusText)
}

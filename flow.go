package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const maxJavaScriptSafeInteger int64 = 1<<53 - 1

type FlowEventView struct {
	Sequence       string `json:"sequence"`
	EventID        string `json:"event_id"`
	CaseID         string `json:"case_id"`
	At             string `json:"at"`
	Type           string `json:"event_type"`
	Actor          string `json:"actor"`
	Recipient      string `json:"recipient,omitempty"`
	Result         string `json:"result,omitempty"`
	SourceRef      string `json:"source_ref,omitempty"`
	ArtifactRef    string `json:"artifact_ref,omitempty"`
	FromState      string `json:"from_state,omitempty"`
	ToState        string `json:"to_state,omitempty"`
	NextAction     string `json:"next_action,omitempty"`
	RelatedEventID string `json:"related_event_id,omitempty"`
	DeliveryID     string `json:"delivery_id,omitempty"`
}

type CaseFlowView struct {
	CaseID     string          `json:"case_id"`
	Events     []FlowEventView `json:"events"`
	Deliveries []DeliveryView  `json:"deliveries"`
}

func makeJSONSafeForJavaScript(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	return rewriteUnsafeJSONIntegers(document, ""), nil
}

func rewriteUnsafeJSONIntegers(value any, field string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = rewriteUnsafeJSONIntegers(child, key)
		}
		return typed
	case []any:
		for i, child := range typed {
			typed[i] = rewriteUnsafeJSONIntegers(child, field)
		}
		return typed
	case json.Number:
		if field != "sequence" && field != "last_sequence" && field != "old_sequence" {
			return typed
		}
		integer, err := typed.Int64()
		if err == nil && (integer > maxJavaScriptSafeInteger || integer < -maxJavaScriptSafeInteger) {
			return typed.String()
		}
		return typed
	default:
		return value
	}
}

func (a *App) cmdFlow(args []string) error {
	if len(args) == 0 || args[0] != "show" {
		return fmt.Errorf("用法：hq flow show --case CASE-ID")
	}
	fs := newLeafParser("flow show")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法：hq flow show --case CASE-ID")
	}
	if err := validateCaseID(*caseID); err != nil {
		return err
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	view := CaseFlowView{CaseID: *caseID, Events: []FlowEventView{}, Deliveries: []DeliveryView{}}
	for _, event := range events {
		if event.CaseID != *caseID {
			continue
		}
		view.Events = append(view.Events, FlowEventView{
			Sequence: strconv.FormatInt(event.Sequence, 10), EventID: event.ID, CaseID: event.CaseID,
			At: event.At, Type: event.Type, Actor: event.Actor, Recipient: event.Recipient,
			Result: event.Result, SourceRef: event.SourceRef, ArtifactRef: event.ArtifactRef,
			FromState: event.FromState, ToState: event.ToState, NextAction: event.NextAction,
			RelatedEventID: event.RelatedEventID, DeliveryID: event.DeliveryID,
		})
	}
	if len(view.Events) == 0 {
		return fmt.Errorf("case 无事件：%s", *caseID)
	}
	deliveries, err := a.Store.Deliveries(a.Config)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if delivery.CaseID == *caseID {
			view.Deliveries = append(view.Deliveries, delivery)
		}
	}
	return a.output(view, fmt.Sprintf("case=%s events=%d deliveries=%d", view.CaseID, len(view.Events), len(view.Deliveries)))
}

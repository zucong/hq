package main

import (
	"context"
	"fmt"
)

type IdentityProvider interface {
	Resolve(Config, string, string) (Actor, error)
}

type contextualIdentityProvider interface {
	ResolveContext(context.Context, Config, string, string) (Actor, error)
}

type DeliveryTransport interface {
	Deliver(string, string) DeliveryAttempt
}

type contextualDeliveryTransport interface {
	DeliverContext(context.Context, string, string) DeliveryAttempt
}

type TransportOutcome string

const (
	transportSent              TransportOutcome = "sent"
	transportDefinitelyNotSent TransportOutcome = "definitely-not-sent"
	transportAmbiguous         TransportOutcome = "ambiguous"
)

type DeliveryAttempt struct {
	Outcome TransportOutcome
	Err     error
}

type herdrIdentityProvider struct {
	Control HerdrControl
}

type herdrDeliveryTransport struct {
	Control HerdrControl
}

func (p herdrIdentityProvider) Resolve(cfg Config, hqRoot, paneID string) (Actor, error) {
	return p.ResolveContext(context.Background(), cfg, hqRoot, paneID)
}

func (p herdrIdentityProvider) ResolveContext(parent context.Context, cfg Config, hqRoot, paneID string) (Actor, error) {
	if paneID == "" {
		return Actor{}, fmt.Errorf("缺少 pane_id，无法识别发件人")
	}
	if p.Control == nil {
		return Actor{}, fmt.Errorf("herdr snapshot provider 未注入")
	}
	ctx, cancel := context.WithTimeout(parent, defaultHerdrSnapshotTimeout)
	defer cancel()
	snapshot, err := p.Control.Snapshot(ctx, HerdrSnapshotScope{WorkspaceLabel: cfg.WorkspaceLabel})
	if err != nil {
		return Actor{}, fmt.Errorf("读取 herdr 会话快照：%w", err)
	}
	binding, err := ResolveLiveBinding(snapshot, cfg, hqRoot, LiveBindingRequest{PaneID: paneID, RequireInteractiveReady: true})
	if err != nil {
		return Actor{}, err
	}
	return Actor{
		Name:       binding.Seat,
		Label:      binding.Rule.Label,
		Department: binding.Rule.Department,
		PaneID:     binding.PaneID,
		CWD:        binding.CWD,
		Rule:       binding.Rule,
	}, nil
}

func (t herdrDeliveryTransport) Deliver(target, message string) DeliveryAttempt {
	return t.DeliverContext(context.Background(), target, message)
}

func (t herdrDeliveryTransport) DeliverContext(ctx context.Context, target, message string) DeliveryAttempt {
	if t.Control == nil {
		return DeliveryAttempt{Outcome: transportDefinitelyNotSent, Err: fmt.Errorf("herdr control 未注入")}
	}
	result := t.Control.Prompt(ctx, target, message)
	if result.Err == nil && result.Outcome == herdrConfirmed {
		return DeliveryAttempt{Outcome: transportSent}
	}
	outcome := transportAmbiguous
	if result.Outcome == herdrDefinitelyNotRun {
		outcome = transportDefinitelyNotSent
	}
	return DeliveryAttempt{Outcome: outcome, Err: result.Err}
}

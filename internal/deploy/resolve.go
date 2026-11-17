package deploy

import (
	"context"
	"database/sql"
	"errors"

	"berthy/internal/state"
)

// RecoveryStep is the public view of a journaled step; specs and receipts are never included.
type RecoveryStep struct {
	Ordinal   int    `json:"ordinal"`
	Kind      string `json:"kind"`
	Role      string `json:"role,omitempty"`
	Name      string `json:"name,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	Phase     string `json:"phase,omitempty"`
}

// RecoveryReport is what an administrator sees about an operation stuck in recovery, before and after resolving it.
type RecoveryReport struct {
	OperationID  string         `json:"operation_id"`
	Kind         string         `json:"kind"`
	App          string         `json:"app"`
	Error        string         `json:"error"`
	SchemaBefore string         `json:"schema_before,omitempty"`
	SchemaAfter  string         `json:"schema_after,omitempty"`
	Steps        []RecoveryStep `json:"steps"`
	Resolved     bool           `json:"resolved"`
}

// ResolveRecovery reports what a stuck operation left in flight and, unless dryRun, finishes it as failed.
func (c *Controller) ResolveRecovery(ctx context.Context, actor Actor, id, reason string, dryRun bool) (RecoveryReport, error) {
	if !actor.Admin {
		return RecoveryReport{}, errResolveAdmin
	}
	if c.Store == nil {
		return RecoveryReport{}, errors.New("state unavailable")
	}
	c.recoverMu.Lock()
	defer c.recoverMu.Unlock()
	op, err := c.Store.Active(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return RecoveryReport{}, errNotRecovering
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	if op.ID != id || op.State != "recovery_required" {
		return RecoveryReport{}, errNotRecovering
	}
	intents, err := c.Store.Intents(ctx, op.ID)
	if err != nil {
		return RecoveryReport{}, err
	}
	report := RecoveryReport{
		OperationID:  op.ID,
		Kind:         op.Kind,
		App:          op.App,
		Error:        op.Err,
		SchemaBefore: op.SchemaBefore,
		SchemaAfter:  op.SchemaAfter,
		Steps:        make([]RecoveryStep, 0, len(intents)),
	}
	for _, i := range intents {
		report.Steps = append(report.Steps, RecoveryStep{Ordinal: i.Ordinal, Kind: i.Kind, Role: i.Role, Name: i.Name, ServiceID: i.ServiceID, Phase: i.Phase})
	}
	if dryRun {
		return report, nil
	}
	err = c.Store.ResolveRecovery(ctx, state.Actor{ID: actor.ID, Name: actor.Name, Remote: actor.Remote}, op.ID, reason)
	if errors.Is(err, state.ErrConflict) {
		return RecoveryReport{}, errNotRecovering
	}
	if err != nil {
		return RecoveryReport{}, err
	}
	report.Resolved = true
	c.reportEnd(ctx, op, state.Outcome{State: "failed", Err: "recovery resolved by admin"}, target{})
	return report, nil
}

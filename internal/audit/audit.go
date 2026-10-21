// Package audit appends to the audit log. Events are written inside the same transaction as the action they
// describe, so an action and its audit record commit or roll back together. The table is append-only.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/audit/store"
)

// ActorType says who performed the action.
type ActorType string

// Actor types.
const (
	System ActorType = "system"
	Player ActorType = "player"
	Staff  ActorType = "staff"
)

// Event is one audited fact. Details must not contain secrets or full personal data.
type Event struct {
	ActorType  ActorType
	ActorID    string // required for staff and players
	Action     string // dotted verb, e.g. "draw.conduct", "wallet.adjust", "kyc.unmask"
	EntityType string
	EntityID   string
	Details    map[string]any
}

// Writer appends events.
type Writer struct{}

// NewWriter returns a Writer.
func NewWriter() *Writer { return &Writer{} }

// Record appends the event using the caller's transaction.
func (*Writer) Record(ctx context.Context, tx pgx.Tx, e Event) error {
	switch e.ActorType {
	case System:
	case Staff, Player:
		if e.ActorID == "" {
			return fmt.Errorf("audit: %s events must name the actor", e.ActorType)
		}
	default:
		return fmt.Errorf("audit: unknown actor type %q", e.ActorType)
	}
	if e.Action == "" || e.EntityType == "" {
		return errors.New("audit: action and entity type are required")
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: encode details: %w", err)
	}
	return store.New(tx).InsertAuditEvent(ctx, store.InsertAuditEventParams{
		ActorType: string(e.ActorType), ActorID: opt(e.ActorID), Action: e.Action,
		EntityType: e.EntityType, EntityID: opt(e.EntityID), Details: raw,
	})
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

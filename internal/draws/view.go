package draws

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/itsivali/lottery-platform/internal/draws/store"
)

// Draw is what is known about a draw. RevealedSeed, the beacon and the certificate exist once it is conducted.
type Draw struct {
	ID              string          `json:"id"`
	Type            string          `json:"draw_type"`
	PeriodStart     string          `json:"period_start"`
	PeriodEnd       string          `json:"period_end"`
	Status          string          `json:"status"`
	FreezeAt        time.Time       `json:"freeze_at"`
	EntryListHash   string          `json:"entry_list_hash,omitempty"`
	SeedCommit      string          `json:"seed_commit,omitempty"`
	BeaconRound     int64           `json:"drand_round,omitempty"`
	RoundAt         *time.Time      `json:"drand_round_at,omitempty"`
	RevealedSeed    string          `json:"revealed_seed,omitempty"`
	Certificate     json.RawMessage `json:"certificate,omitempty"`
	CertificateHash string          `json:"certificate_hash,omitempty"`
	VoidedReason    string          `json:"voided_reason,omitempty"`
	RerunOf         string          `json:"rerun_of,omitempty"`
	Entries         int             `json:"entries"`
	Signoffs        []Signoff       `json:"signoffs"`
	Results         []Result        `json:"results"`
}

// Signoff is one official's step.
type Signoff struct {
	Step      string    `json:"step"`
	Official  string    `json:"official"`
	Signature string    `json:"attestation"`
	SignedAt  time.Time `json:"signed_at"`
}

// Result is one awarded rank.
type Result struct {
	Rank      int    `json:"rank"`
	AmountKES int64  `json:"amount_kes"`
	EntryNo   int    `json:"entry_no"`
	UserID    string `json:"user_id"`
}

// Get returns one draw with its sign-offs and results.
func (s *Service) Get(ctx context.Context, id string) (Draw, error) {
	q := store.New(s.pool)
	d, err := q.GetDraw(ctx, id)
	var pe *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pe) && pe.Code == "22P02") {
		return Draw{}, ErrNotFound
	}
	if err != nil {
		return Draw{}, err
	}
	out := Draw{ID: d.ID, Type: d.DrawType, PeriodStart: d.PeriodStart.Format(time.DateOnly), PeriodEnd: d.PeriodEnd.Format(time.DateOnly),
		Status: d.Status, FreezeAt: FreezeTime(d.PeriodEnd), EntryListHash: deref(d.EntryListHash), SeedCommit: deref(d.SeedCommit),
		BeaconRound: derefInt(d.BeaconRound), RevealedSeed: deref(d.RevealedSeed), Certificate: d.CertificateJson,
		CertificateHash: deref(d.CertificateHash), VoidedReason: deref(d.VoidedReason), RerunOf: deref(d.RerunOf),
		Signoffs: []Signoff{}, Results: []Result{}}
	if d.BeaconRound != nil {
		t := s.roundTime(d)
		out.RoundAt = &t
	}
	entries, err := q.Entries(ctx, d.ID)
	if err != nil {
		return Draw{}, err
	}
	out.Entries = len(entries)
	signs, err := q.Signoffs(ctx, d.ID)
	if err != nil {
		return Draw{}, err
	}
	for _, so := range signs {
		out.Signoffs = append(out.Signoffs, Signoff{Step: so.Step, Official: so.Official, Signature: so.Signature, SignedAt: so.SignedAt})
	}
	results, err := q.Results(ctx, d.ID)
	if err != nil {
		return Draw{}, err
	}
	for _, r := range results {
		out.Results = append(out.Results, Result{Rank: int(r.PrizeRank), AmountKES: r.AmountKes, EntryNo: int(r.EntryNo), UserID: r.UserID})
	}
	return out, nil
}

// List returns the most recent draws, newest period first.
func (s *Service) List(ctx context.Context, limit int) ([]Draw, error) {
	ids, err := store.New(s.pool).ListDraws(ctx, int32(limit)) //nolint:gosec // callers pass small limits
	if err != nil {
		return nil, err
	}
	out := make([]Draw, 0, len(ids))
	for _, id := range ids {
		d, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

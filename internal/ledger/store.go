package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/itsivali/lottery-platform/internal/ledger/store"
)

// ErrAlreadyPosted means the same (kind, reference) event was already journalled; the repeat is refused.
var ErrAlreadyPosted = errors.New("ledger: event already posted")

// ErrNoTaxRule means no tax_rules version applies; callers decide whether that means "untaxed" or an error.
var ErrNoTaxRule = errors.New("ledger: no tax rule in force")

const uniqueViolation = "23505"

// Store writes journals. It caches the chart of accounts, which is immutable apart from additive migrations.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]string
}

// NewStore returns a Store.
func NewStore() *Store { return &Store{} }

func (s *Store) accountIDs(ctx context.Context, tx pgx.Tx) (map[string]string, error) {
	s.mu.RLock()
	m := s.accounts
	s.mu.RUnlock()
	if m != nil {
		return m, nil
	}
	rows, err := store.New(tx).ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: list accounts: %w", err)
	}
	m = make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Code] = r.ID
	}
	s.mu.Lock()
	s.accounts = m
	s.mu.Unlock()
	return m, nil
}

// Post validates and writes a journal inside the caller's transaction and returns its id. The database re-checks
// balance at commit; a journal for an event that was already posted yields ErrAlreadyPosted.
func (s *Store) Post(ctx context.Context, tx pgx.Tx, j Journal) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}
	ids, err := s.accountIDs(ctx, tx)
	if err != nil {
		return "", err
	}
	q := store.New(tx)
	id, err := q.InsertJournal(ctx, store.InsertJournalParams{Kind: j.Kind, ReferenceType: j.RefType, ReferenceID: j.RefID})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == uniqueViolation {
			return "", ErrAlreadyPosted
		}
		return "", fmt.Errorf("ledger: insert journal: %w", err)
	}
	for _, e := range j.Entries {
		acct, ok := ids[e.Account]
		if !ok {
			return "", fmt.Errorf("ledger: account %q is not in the chart", e.Account)
		}
		var user *string
		if e.UserID != "" {
			u := e.UserID
			user = &u
		}
		if err := q.InsertEntry(ctx, store.InsertEntryParams{
			JournalID: id, AccountID: acct, UserID: user, Direction: string(e.Dir), AmountKes: e.AmountKES,
		}); err != nil {
			return "", fmt.Errorf("ledger: insert entry: %w", err)
		}
	}
	return id, nil
}

// ActiveRule returns the tax rule version in force on the given date.
func (s *Store) ActiveRule(ctx context.Context, tx pgx.Tx, code string, on time.Time) (TaxRule, error) {
	row, err := store.New(tx).ActiveTaxRule(ctx, store.ActiveTaxRuleParams{
		Code: code, EffectiveFrom: pgtype.Date{Time: on, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return TaxRule{}, ErrNoTaxRule
	}
	if err != nil {
		return TaxRule{}, fmt.Errorf("ledger: active rule: %w", err)
	}
	r, err := ParseRate(row.Rate)
	if err != nil {
		return TaxRule{}, err
	}
	return TaxRule{Code: row.Code, Rate: r, Rounding: Rounding(row.Rounding)}, nil
}

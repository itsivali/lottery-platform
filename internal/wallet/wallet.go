// Package wallet owns player balances. Every balance change is one atomic conditional UPDATE plus an append-only
// wallet_transactions row plus a balanced ledger journal, all inside the caller's transaction.
package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/wallet/store"
)

// Errors returned by the service.
var (
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	ErrAlreadyPosted     = ledger.ErrAlreadyPosted
)

// Poster writes a journal and returns its id. ledger.Store implements it.
type Poster interface {
	Post(ctx context.Context, tx pgx.Tx, j ledger.Journal) (string, error)
}

// Service applies postings to wallets.
type Service struct{ ledger Poster }

// NewService returns a Service that journals through p.
func NewService(p Poster) *Service { return &Service{ledger: p} }

// Apply writes the posting's journal, moves the wallet by the posting's net delta and appends its transactions.
// It returns the new balance. A debit that the balance cannot cover returns ErrInsufficientFunds and the caller
// must roll back; an event that was already posted returns ErrAlreadyPosted.
func (s *Service) Apply(ctx context.Context, tx pgx.Tx, p ledger.Posting) (int64, error) {
	if p.UserID == "" {
		return 0, errors.New("wallet: posting has no user")
	}
	if err := p.Journal.Validate(); err != nil {
		return 0, err
	}
	q := store.New(tx)
	journalID, err := s.ledger.Post(ctx, tx, p.Journal)
	if err != nil {
		return 0, err
	}
	if err := q.EnsureWallet(ctx, p.UserID); err != nil {
		return 0, fmt.Errorf("wallet: ensure: %w", err)
	}
	var balance int64
	switch delta := p.WalletDelta(); {
	case delta < 0:
		balance, err = q.DebitWallet(ctx, store.DebitWalletParams{AmountKes: -delta, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrInsufficientFunds
		}
	case delta > 0:
		balance, err = q.CreditWallet(ctx, store.CreditWalletParams{AmountKes: delta, UserID: p.UserID})
	default:
		balance, err = q.LockWalletBalance(ctx, p.UserID)
	}
	if err != nil {
		return 0, fmt.Errorf("wallet: update balance: %w", err)
	}
	refType, refID := p.Journal.RefType, p.Journal.RefID
	for _, w := range p.WalletTxs {
		if err := q.InsertWalletTx(ctx, store.InsertWalletTxParams{
			UserID: p.UserID, JournalID: journalID, Kind: w.Kind, AmountKes: w.AmountKES,
			ReferenceType: &refType, ReferenceID: &refID,
		}); err != nil {
			return 0, fmt.Errorf("wallet: insert transaction: %w", err)
		}
	}
	return balance, nil
}

// Balance returns the wallet balance, or 0 for a player who has none yet.
func (s *Service) Balance(ctx context.Context, tx pgx.Tx, user string) (int64, error) {
	b, err := store.New(tx).GetBalance(ctx, user)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return b, err
}

// ReverseDeposit claws back a credited top-up under the wallet row lock, so the amount taken from the wallet and the
// shortfall booked as player debt are decided from the same balance that is then debited. exciseDeducted is the
// excise that was taken out of the original credit (the player never received it).
func (s *Service) ReverseDeposit(ctx context.Context, tx pgx.Tx, paymentID, user string, gross, exciseDeducted int64) (ledger.Posting, int64, error) {
	q := store.New(tx)
	if err := q.EnsureWallet(ctx, user); err != nil {
		return ledger.Posting{}, 0, fmt.Errorf("wallet: ensure: %w", err)
	}
	bal, err := q.LockWalletBalance(ctx, user)
	if err != nil {
		return ledger.Posting{}, 0, fmt.Errorf("wallet: lock: %w", err)
	}
	p, err := ledger.DepositReversal(paymentID, user, gross, exciseDeducted, bal)
	if err != nil {
		return ledger.Posting{}, 0, err
	}
	newBal, err := s.Apply(ctx, tx, p)
	return p, newBal, err
}

// Debt returns what the player owes after reversals (0 when nothing is owed). Play and withdrawals must be refused
// while it is positive.
func (s *Service) Debt(ctx context.Context, tx pgx.Tx, user string) (int64, error) {
	return store.New(tx).PlayerDebt(ctx, user)
}

// RecoverDebt takes what the player owes out of their wallet, up to the balance, under the wallet row lock. It is
// keyed on the deposit (paymentID) that funds it and called in the same transaction as that credit, so a deposit
// recovers at most once. It returns the amount recovered.
func (s *Service) RecoverDebt(ctx context.Context, tx pgx.Tx, user, paymentID string) (int64, error) {
	q := store.New(tx)
	if err := q.EnsureWallet(ctx, user); err != nil {
		return 0, fmt.Errorf("wallet: ensure: %w", err)
	}
	bal, err := q.LockWalletBalance(ctx, user)
	if err != nil {
		return 0, fmt.Errorf("wallet: lock: %w", err)
	}
	debt, err := q.PlayerDebt(ctx, user)
	if err != nil {
		return 0, err
	}
	amount := min(debt, bal)
	if amount <= 0 {
		return 0, nil
	}
	p, err := ledger.DebtRecovery(paymentID, user, amount)
	if err != nil {
		return 0, err
	}
	if _, err := s.Apply(ctx, tx, p); err != nil {
		return 0, err
	}
	return amount, nil
}

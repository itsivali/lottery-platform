// Package game sells plays. A play costs the price of the odds version in force on the play date, debits the wallet,
// issues a ticket (at most 4 per player per Nairobi day; the database enforces the cap) and spins once with the
// player's provably fair seed chain (package fair). A spin on a lucky slot marks the day lucky, which doubles the day's
// draw chances (owner, 2026-10-24). Everything about one play commits in one transaction or not at all.
package game

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/game/fair"
	"github.com/itsivali/lottery-platform/internal/game/store"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

// DailyPlays is the most a player may play in one Nairobi day (also a database constraint).
const DailyPlays = 4

// Errors.
var (
	ErrDailyCap          = errors.New("game: no plays left today")
	ErrInDebt            = errors.New("game: the player owes money from a reversed deposit")
	ErrInsufficientFunds = errors.New("game: balance below the price of a play")
	ErrIdempotencyKey    = errors.New("game: an idempotency key is required")
	ErrNotFound          = errors.New("game: not found")
	ErrNoOdds            = errors.New("game: no odds version in force")
)

// Identity is what the game needs from identity.
type Identity interface {
	AssertCanPlay(ctx context.Context, tx pgx.Tx, userID string) error
}

// RG is what the game needs from responsible gaming.
type RG interface {
	AssertNotRestricted(ctx context.Context, tx pgx.Tx, userID string) error
}

// Wallet debits stakes and reports player debt.
type Wallet interface {
	Apply(ctx context.Context, tx pgx.Tx, p ledger.Posting) (int64, error)
	Balance(ctx context.Context, tx pgx.Tx, user string) (int64, error)
	Debt(ctx context.Context, tx pgx.Tx, user string) (int64, error)
}

// Service runs plays and seed chains.
type Service struct {
	pool   *pgxpool.Pool
	clk    clock.Clock
	ids    Identity
	rg     RG
	wallet Wallet
	keys   *crypto.Keyring
	// lowBalance is the wallet's insufficient-funds error.
	lowBalance error
}

// NewService returns a Service. lowBalance is the wallet error for a balance below the stake.
func NewService(pool *pgxpool.Pool, clk clock.Clock, ids Identity, r RG, w Wallet, keys *crypto.Keyring, lowBalance error) *Service {
	return &Service{pool: pool, clk: clk, ids: ids, rg: r, wallet: w, keys: keys, lowBalance: lowBalance}
}

// Spin is the provably fair part of a play.
type Spin struct {
	ChainID    string
	Commitment string
	ClientSeed string
	Nonce      int64
	Slot       int
	Lucky      bool
	Slots      int
	LuckySlots int
}

// Play is one ticket and its spin.
type Play struct {
	TicketID  string
	PlayDate  time.Time
	Seq       int
	PriceKES  int64
	Status    string
	CreatedAt time.Time
	Spin      Spin
}

// Today is the player's play day so far.
type Today struct {
	PlayDate  time.Time
	Plays     int
	PlaysLeft int
	Lucky     bool
	Chances   int
	PriceKES  int64
	Tickets   []Play
}

// Result is what a play returns: the play, the day after it and the new balance.
type Result struct {
	Play       Play
	Today      Today
	BalanceKES int64
	Replayed   bool // the same idempotency key was already used: nothing new was charged
}

// playDate is the Nairobi calendar day of t, as a date (midnight UTC) for the database.
func playDate(t time.Time) time.Time {
	y, m, d := clock.InNairobi(t).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func seedAAD(userID string) []byte { return []byte("game.seed_chain:" + userID) }

// Play sells one play to the player.
func (s *Service) Play(ctx context.Context, userID, key string) (Result, error) {
	if key == "" {
		return Result{}, ErrIdempotencyKey
	}
	var res Result
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if id, err := q.TicketByKey(ctx, store.TicketByKeyParams{UserID: userID, IdempotencyKey: key}); err == nil {
			p, err := s.load(ctx, q, id)
			if err != nil {
				return err
			}
			balance, err := s.wallet.Balance(ctx, tx, userID)
			res = Result{Play: p, BalanceKES: balance, Replayed: true}
			return err
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		day := playDate(now)
		if err := s.ids.AssertCanPlay(ctx, tx, userID); err != nil {
			return err
		}
		if err := s.rg.AssertNotRestricted(ctx, tx, userID); err != nil {
			return err
		}
		if debt, err := s.wallet.Debt(ctx, tx, userID); err != nil {
			return err
		} else if debt > 0 {
			return ErrInDebt
		}
		odds, err := q.OddsFor(ctx, day)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoOdds
		} else if err != nil {
			return err
		}
		// the day row serialises the player's concurrent plays
		if err := q.EnsurePlayerDay(ctx, store.EnsurePlayerDayParams{UserID: userID, PlayDate: day, UpdatedAt: now}); err != nil {
			return err
		}
		pd, err := q.LockPlayerDay(ctx, store.LockPlayerDayParams{UserID: userID, PlayDate: day})
		if err != nil {
			return err
		}
		if pd.PaidPlays >= DailyPlays {
			return ErrDailyCap
		}
		chain, seed, err := s.lockChain(ctx, q, userID, now)
		if err != nil {
			return err
		}
		slot, err := fair.Slot(seed, chain.ClientSeed, chain.NextNonce, int(odds.SlotCount))
		if err != nil {
			return err
		}
		lucky := slot < int(odds.LuckySlots)
		seq := pd.PaidPlays + 1
		ticketID, err := q.InsertTicket(ctx, store.InsertTicketParams{UserID: userID, PlayDate: day, Seq: seq,
			PriceKes: odds.PriceKes, IdempotencyKey: key, CreatedAt: now})
		if err != nil {
			return err
		}
		posting, err := ledger.Stake(ticketID, userID, odds.PriceKes)
		if err != nil {
			return err
		}
		balance, err := s.wallet.Apply(ctx, tx, posting)
		if s.lowBalance != nil && errors.Is(err, s.lowBalance) {
			return ErrInsufficientFunds
		} else if err != nil {
			return err
		}
		if err := q.InsertSpin(ctx, store.InsertSpinParams{TicketID: ticketID, ChainID: chain.ID, ClientSeed: chain.ClientSeed,
			Nonce: chain.NextNonce, Slot: int16(slot), Lucky: lucky, OddsConfigID: odds.ID, CreatedAt: now}); err != nil { //nolint:gosec // slot < slot_count, a smallint
			return err
		}
		if err := q.AdvanceNonce(ctx, chain.ID); err != nil {
			return err
		}
		if err := q.RecordPlay(ctx, store.RecordPlayParams{Lucky: lucky, At: now, UserID: userID, PlayDate: day}); err != nil {
			return err
		}
		p, err := s.load(ctx, q, ticketID)
		if err != nil {
			return err
		}
		today, err := s.today(ctx, q, userID, day)
		if err != nil {
			return err
		}
		res = Result{Play: p, Today: today, BalanceKES: balance}
		return nil
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "tickets_user_idempotency_key" {
		// the same key raced another request: answer with the play that won
		return s.Play(ctx, userID, key)
	}
	if err != nil {
		return Result{}, err
	}
	if res.Replayed {
		today, err := s.Today(ctx, userID)
		if err != nil {
			return Result{}, err
		}
		res.Today = today
	}
	return res, nil
}

// activeChain is a locked chain with its decrypted seed.
type activeChain struct {
	ID         string
	ClientSeed string
	NextNonce  int64
}

// lockChain locks the player's active seed chain, creating one when there is none.
func (s *Service) lockChain(ctx context.Context, q *store.Queries, userID string, now time.Time) (activeChain, []byte, error) {
	c, err := q.LockActiveChain(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := s.newChain(ctx, q, userID, "", now); err != nil {
			return activeChain{}, nil, err
		}
		c, err = q.LockActiveChain(ctx, userID)
	}
	if err != nil {
		return activeChain{}, nil, err
	}
	seed, err := s.keys.Decrypt(c.SeedEnc, seedAAD(userID))
	if err != nil {
		return activeChain{}, nil, fmt.Errorf("game: seed chain %s: %w", c.ID, err)
	}
	return activeChain{ID: c.ID, ClientSeed: c.ClientSeed, NextNonce: c.NextNonce}, seed, nil
}

func (s *Service) newChain(ctx context.Context, q *store.Queries, userID, clientSeed string, now time.Time) (string, error) {
	if clientSeed == "" {
		cs, err := fair.NewClientSeed()
		if err != nil {
			return "", err
		}
		clientSeed = cs
	}
	if !fair.ValidClientSeed(clientSeed) {
		return "", fair.ErrBadClientSeed
	}
	seed, err := fair.NewServerSeed()
	if err != nil {
		return "", err
	}
	enc, err := s.keys.Encrypt(seed, seedAAD(userID))
	if err != nil {
		return "", err
	}
	return q.InsertChain(ctx, store.InsertChainParams{UserID: userID, SeedEnc: enc, Commitment: fair.Commitment(seed), ClientSeed: clientSeed, CreatedAt: now})
}

func (s *Service) load(ctx context.Context, q *store.Queries, ticketID string) (Play, error) {
	r, err := q.GetPlay(ctx, ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Play{}, ErrNotFound
	}
	if err != nil {
		return Play{}, err
	}
	return Play{TicketID: r.ID, PlayDate: r.PlayDate, Seq: int(r.Seq), PriceKES: r.PriceKes, Status: r.Status, CreatedAt: r.CreatedAt,
		Spin: Spin{ChainID: r.ChainID, Commitment: r.Commitment, ClientSeed: r.ClientSeed, Nonce: r.Nonce, Slot: int(r.Slot),
			Lucky: r.Lucky, Slots: int(r.SlotCount), LuckySlots: int(r.LuckySlots)}}, nil
}

// Today returns the player's current Nairobi play day.
func (s *Service) Today(ctx context.Context, userID string) (Today, error) {
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return Today{}, err
	}
	return s.today(ctx, store.New(s.pool), userID, playDate(now))
}

func (s *Service) today(ctx context.Context, q *store.Queries, userID string, day time.Time) (Today, error) {
	odds, err := q.OddsFor(ctx, day)
	if errors.Is(err, pgx.ErrNoRows) {
		return Today{}, ErrNoOdds
	} else if err != nil {
		return Today{}, err
	}
	t := Today{PlayDate: day, PriceKES: odds.PriceKes, PlaysLeft: DailyPlays, Tickets: []Play{}}
	pd, err := q.GetPlayerDay(ctx, store.GetPlayerDayParams{UserID: userID, PlayDate: day})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Today{}, err
	}
	t.Plays, t.Lucky, t.PlaysLeft = int(pd.PaidPlays), pd.Lucky, DailyPlays-int(pd.PaidPlays)
	if t.Plays > 0 {
		tiers, err := q.TiersFor(ctx, day)
		if err != nil {
			return Today{}, err
		}
		for _, tr := range tiers {
			if int(tr.Plays) == t.Plays {
				t.Chances = int(tr.Chances)
			}
		}
		if t.Lucky {
			t.Chances *= 2
		}
	}
	ids, err := q.PlaysOn(ctx, store.PlaysOnParams{UserID: userID, PlayDate: day})
	if err != nil {
		return Today{}, err
	}
	for _, id := range ids {
		p, err := s.load(ctx, q, id)
		if err != nil {
			return Today{}, err
		}
		t.Tickets = append(t.Tickets, p)
	}
	return t, nil
}

// Chain is a seed chain as the player sees it. RevealedSeed is set once the chain has been rotated.
type Chain struct {
	ID           string
	Commitment   string
	ClientSeed   string
	NextNonce    int64
	RevealedSeed string
	CreatedAt    time.Time
	RevealedAt   *time.Time
}

// Fairness is the player's active chain (its seed still secret) and the most recent rotated chains.
type Fairness struct {
	Active  Chain
	Rotated []Chain
}

// Fairness returns the active chain, creating it first if the player has none, so the commitment is always shown
// before any spin uses the seed.
func (s *Service) Fairness(ctx context.Context, userID string) (Fairness, error) {
	var f Fairness
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := q.GetActiveChain(ctx, userID); errors.Is(err, pgx.ErrNoRows) {
			if _, err := s.newChain(ctx, q, userID, "", now); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		f, err = s.fairness(ctx, q, userID)
		return err
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" { // two first requests raced to create the chain
		return s.Fairness(ctx, userID)
	}
	return f, err
}

func (s *Service) fairness(ctx context.Context, q *store.Queries, userID string) (Fairness, error) {
	a, err := q.GetActiveChain(ctx, userID)
	if err != nil {
		return Fairness{}, err
	}
	f := Fairness{Active: Chain{ID: a.ID, Commitment: a.Commitment, ClientSeed: a.ClientSeed, NextNonce: a.NextNonce, CreatedAt: a.CreatedAt}, Rotated: []Chain{}}
	rows, err := q.RotatedChains(ctx, userID)
	if err != nil {
		return Fairness{}, err
	}
	for _, r := range rows {
		c := Chain{ID: r.ID, Commitment: r.Commitment, ClientSeed: r.ClientSeed, NextNonce: r.NextNonce, CreatedAt: r.CreatedAt, RevealedAt: r.RevealedAt}
		if r.RevealedSeed != nil {
			c.RevealedSeed = *r.RevealedSeed
		}
		f.Rotated = append(f.Rotated, c)
	}
	return f, nil
}

// Rotate reveals the active chain's server seed and starts a new chain, with the player's client seed if given.
func (s *Service) Rotate(ctx context.Context, userID, clientSeed string) (Fairness, error) {
	if clientSeed != "" && !fair.ValidClientSeed(clientSeed) {
		return Fairness{}, fair.ErrBadClientSeed
	}
	var f Fairness
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		c, err := q.LockActiveChain(ctx, userID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			seed, err := s.keys.Decrypt(c.SeedEnc, seedAAD(userID))
			if err != nil {
				return err
			}
			if fair.Commitment(seed) != c.Commitment {
				return fmt.Errorf("game: seed chain %s does not match its commitment", c.ID)
			}
			revealed := hex.EncodeToString(seed)
			if err := q.RotateChain(ctx, store.RotateChainParams{At: &now, RevealedSeed: &revealed, ID: c.ID}); err != nil {
				return err
			}
		}
		if _, err := s.newChain(ctx, q, userID, clientSeed, now); err != nil {
			return err
		}
		f, err = s.fairness(ctx, q, userID)
		return err
	})
	return f, err
}

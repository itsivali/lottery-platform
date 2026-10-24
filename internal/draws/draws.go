// Package draws runs the daily, weekly, monthly and quarterly draws (docs/checkpoints/phase-4.md).
//
// Lifecycle: open (the current period) → frozen (00:05 Nairobi after the period: entries numbered and hashed, a secret
// company seed committed, the drand round fixed: the first one at or after 09:00) → signed (two different officials,
// before that round is published) → conducted (the verified beacon and the revealed seed pick the winners) → approved
// (two officials; the pool moves from stakes revenue to the prize pool) → paid (phase 5). A frozen or signed draw may be
// voided before its round, and one not signed in time is voided; either way it is re-run as a new draw with a new seed
// and round. The database refuses any other transition (migration 00021).
//
// Entries: each chance a player day earned (its chance tier, doubled on a lucky day) is one entry in every draw whose
// period contains the day. The entry list hash is SHA-256 over the lines "entry_no|user_id|play_date|ticket_id\n" in
// entry order. Winners: key = SHA-256(drand randomness ‖ company seed ‖ entry list hash ‖ draw id); the i-th pick is
// the first 64-bit big-endian word w of HMAC-SHA256(key, "pick:i:round") with w < 2^64 - (2^64 mod n), giving entry
// w mod n + 1. An entry of a player who already won is skipped (one prize per player per draw).
package draws

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/draws/drand"
	"github.com/itsivali/lottery-platform/internal/draws/store"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

// Draw types.
var Types = []string{"daily", "weekly", "monthly", "quarterly"}

// Officials needed for each sign-off step.
const Officials = 2

// Times of day (Nairobi) after a period ends.
const (
	freezeAt = 5 * time.Minute // 00:05
	roundAt  = 9 * time.Hour   // 09:00
)

// Errors.
var (
	ErrNotFound       = errors.New("draws: not found")
	ErrState          = errors.New("draws: the draw is not in a state that allows this")
	ErrTooLate        = errors.New("draws: the drand round has been published; the outcome is public")
	ErrAlreadySigned  = errors.New("draws: this official has already signed this step")
	ErrOfficial       = errors.New("draws: an official's name is required")
	ErrNoPrizeVersion = errors.New("draws: no prize structure in force")
)

// Beacon fetches verified drand rounds.
type Beacon interface {
	Chain() drand.Chain
	Fetch(ctx context.Context, now time.Time, round uint64) (drand.Beacon, error)
}

// Ledger posts journals that touch no wallet.
type Ledger interface {
	Post(ctx context.Context, tx pgx.Tx, j ledger.Journal) (string, error)
}

// Service runs draws.
type Service struct {
	pool   *pgxpool.Pool
	clk    clock.Clock
	keys   *crypto.Keyring
	beacon Beacon
	ledger Ledger
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, clk clock.Clock, keys *crypto.Keyring, b Beacon, l Ledger) *Service {
	return &Service{pool: pool, clk: clk, keys: keys, beacon: b, ledger: l}
}

// nairobiDate is t's Nairobi calendar day as a date (midnight UTC).
func nairobiDate(t time.Time) time.Time {
	y, m, d := clock.InNairobi(t).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// at is the instant of a time of day in Nairobi on a date.
func at(date time.Time, offset time.Duration) time.Time {
	y, m, d := date.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, clock.Nairobi).Add(offset).UTC()
}

// Period is the draw period of drawType containing day.
func Period(drawType string, day time.Time) (time.Time, time.Time, error) {
	y, m, _ := day.Date()
	switch drawType {
	case "daily":
		return day, day, nil
	case "weekly":
		start := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // Monday
		return start, start.AddDate(0, 0, 6), nil
	case "monthly":
		start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, -1), nil
	case "quarterly":
		start := time.Date(y, ((m-1)/3)*3+1, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 3, -1), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("draws: unknown draw type %q", drawType)
}

// FreezeTime is when a period's entries freeze: 00:05 Nairobi the day after it ends.
func FreezeTime(periodEnd time.Time) time.Time { return at(periodEnd.AddDate(0, 0, 1), freezeAt) }

// roundFor picks the drand round: the first at or after 09:00 Nairobi the day after the period, or, when that has
// passed (a late freeze or a re-run), the next 09:00 at least an hour away, so the officials always have time to sign.
func (s *Service) roundFor(periodEnd, now time.Time) uint64 {
	target := at(periodEnd.AddDate(0, 0, 1), roundAt)
	for !target.After(now.Add(time.Hour)) {
		target = at(nairobiDate(target).AddDate(0, 0, 1), roundAt)
	}
	return s.beacon.Chain().RoundAt(target)
}

// Schedule does whatever is due: opens the current periods' draws, freezes those whose period has ended, voids and
// re-runs those not signed before their round, and conducts signed draws whose round is published. It runs every few
// minutes in the worker; each draw is handled in its own transaction and one failure does not stop the others.
func (s *Service) Schedule(ctx context.Context) error {
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return err
	}
	q := store.New(s.pool)
	today := nairobiDate(now)
	var errs []error
	for _, t := range Types {
		start, end, _ := Period(t, today)
		if err := q.EnsureOpenDraw(ctx, store.EnsureOpenDrawParams{DrawType: t, PeriodStart: start, PeriodEnd: end, At: now}); err != nil {
			errs = append(errs, err)
		}
	}
	each := func(status string, f func(store.DrawsDraw) error) {
		ids, err := q.DrawsInStatus(ctx, status)
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, id := range ids {
			d, err := q.GetDraw(ctx, id)
			if err == nil {
				err = f(d)
			}
			if err != nil {
				slog.ErrorContext(ctx, "draw step failed", "draw", id, "status", status, "err", err)
				errs = append(errs, fmt.Errorf("draw %s: %w", id, err))
			}
		}
	}
	each("open", func(d store.DrawsDraw) error {
		if now.Before(FreezeTime(d.PeriodEnd)) {
			return nil
		}
		_, err := s.Freeze(ctx, d.ID)
		return err
	})
	each("frozen", func(d store.DrawsDraw) error {
		if now.Before(s.roundTime(d)) {
			return nil
		}
		_, err := s.voidAndRerun(ctx, d.ID, "", "not signed by two officials before the drand round")
		return err
	})
	each("signed", func(d store.DrawsDraw) error {
		if now.Before(s.roundTime(d)) {
			return nil
		}
		_, err := s.Conduct(ctx, d.ID)
		if errors.Is(err, drand.ErrNotYet) || errors.Is(err, drand.ErrUnavailable) {
			slog.WarnContext(ctx, "drand round not available yet; trying again", "draw", d.ID, "err", err)
			return nil
		}
		return err
	})
	return errors.Join(errs...)
}

func (s *Service) roundTime(d store.DrawsDraw) time.Time {
	if d.BeaconRound == nil {
		return time.Time{}
	}
	return s.beacon.Chain().TimeOf(uint64(*d.BeaconRound)) //nolint:gosec // rounds are positive (CHECK)
}

// Freeze closes a draw's entry list. A draw with no entries is voided.
func (s *Service) Freeze(ctx context.Context, drawID string) (Draw, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		d, err := lock(ctx, q, drawID)
		if err != nil {
			return err
		}
		if d.Status != "open" {
			return ErrState
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		return s.freezeTx(ctx, tx, d, now)
	})
	if err != nil {
		return Draw{}, err
	}
	return s.Get(ctx, drawID)
}

func (s *Service) freezeTx(ctx context.Context, tx pgx.Tx, d store.DrawsDraw, now time.Time) error {
	q := store.New(tx)
	days, err := q.PlayerDaysIn(ctx, store.PlayerDaysInParams{PeriodStart: d.PeriodStart, PeriodEnd: d.PeriodEnd})
	if err != nil {
		return err
	}
	tiers := map[time.Time]map[int16]int16{}
	var rows []store.InsertEntriesParams
	h := sha256.New()
	for _, pd := range days {
		t, ok := tiers[pd.PlayDate]
		if !ok {
			list, err := q.TiersOn(ctx, pd.PlayDate)
			if err != nil {
				return err
			}
			t = map[int16]int16{}
			for _, tr := range list {
				t[tr.Plays] = tr.Chances
			}
			tiers[pd.PlayDate] = t
		}
		chances := int(t[pd.PaidPlays])
		if pd.Lucky {
			chances *= 2
		}
		for range chances {
			n := int32(len(rows) + 1) //nolint:gosec // entries stay far below 2^31
			rows = append(rows, store.InsertEntriesParams{DrawID: d.ID, EntryNo: n, UserID: pd.UserID, PlayDate: pd.PlayDate, TicketID: pd.TicketID})
			_, _ = fmt.Fprintf(h, "%d|%s|%s|%s\n", n, pd.UserID, pd.PlayDate.Format(time.DateOnly), pd.TicketID)
		}
	}
	if len(rows) == 0 {
		return q.VoidDraw(ctx, store.VoidDrawParams{Reason: strPtr("no entries"), At: now, ID: d.ID})
	}
	if _, err := q.InsertEntries(ctx, rows); err != nil {
		return err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	enc, err := s.keys.Encrypt(seed, seedAAD(d.ID))
	if err != nil {
		return err
	}
	commit := sha256.Sum256(seed)
	round := int64(s.roundFor(d.PeriodEnd, now)) //nolint:gosec // drand rounds stay far below 2^63
	return q.Freeze(ctx, store.FreezeParams{EntryListHash: strPtr(hex.EncodeToString(h.Sum(nil))), SeedCommit: strPtr(hex.EncodeToString(commit[:])),
		SeedEnc: enc, BeaconRound: &round, At: &now, ID: d.ID})
}

func seedAAD(drawID string) []byte { return []byte("draws.seed:" + drawID) }

func strPtr(s string) *string { return &s }

func lock(ctx context.Context, q *store.Queries, id string) (store.DrawsDraw, error) {
	d, err := q.LockDraw(ctx, id)
	var pe *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pe) && pe.Code == "22P02") {
		return d, ErrNotFound
	}
	return d, err
}

// attestation is what an official's sign-off commits to: the step, the draw and everything fixed about it so far.
func attestation(step, official string, d store.DrawsDraw) string {
	fields := []string{step, d.ID, d.DrawType, d.PeriodStart.Format(time.DateOnly), d.PeriodEnd.Format(time.DateOnly), deref(d.EntryListHash),
		deref(d.SeedCommit), strconv.FormatInt(derefInt(d.BeaconRound), 10), deref(d.CertificateHash), official}
	sum := sha256.Sum256([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(sum[:])
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// signoff records one official's step and reports how many different officials have signed it.
func signoff(ctx context.Context, q *store.Queries, step, official string, d store.DrawsDraw, now time.Time) (int, error) {
	err := q.InsertSignoff(ctx, store.InsertSignoffParams{DrawID: d.ID, Step: step, Official: official, Signature: attestation(step, official, d), SignedAt: now})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return 0, ErrAlreadySigned
	}
	if err != nil {
		return 0, err
	}
	n, err := q.SignoffCount(ctx, store.SignoffCountParams{DrawID: d.ID, Step: step})
	return int(n), err
}

// Sign records an official's signature on a frozen draw's entry list, seed commitment and round. The second
// different official makes it signed. Signing closes when the round is published.
func (s *Service) Sign(ctx context.Context, drawID, official string) (Draw, error) {
	if strings.TrimSpace(official) == "" {
		return Draw{}, ErrOfficial
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		d, err := lock(ctx, q, drawID)
		if err != nil {
			return err
		}
		if d.Status != "frozen" {
			return ErrState
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(s.roundTime(d)) {
			return ErrTooLate
		}
		n, err := signoff(ctx, q, "sign", official, d, now)
		if err != nil || n < Officials {
			return err
		}
		return q.SetStatus(ctx, store.SetStatusParams{Status: "signed", At: now, ID: d.ID})
	})
	if err != nil {
		return Draw{}, err
	}
	return s.Get(ctx, drawID)
}

// Void voids a frozen or signed draw before its round is published and re-runs it. It returns the re-run.
func (s *Service) Void(ctx context.Context, drawID, official, reason string) (Draw, error) {
	if strings.TrimSpace(official) == "" || strings.TrimSpace(reason) == "" {
		return Draw{}, ErrOfficial
	}
	return s.voidAndRerun(ctx, drawID, official, reason)
}

func (s *Service) voidAndRerun(ctx context.Context, drawID, official, reason string) (Draw, error) {
	var rerun string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		d, err := lock(ctx, q, drawID)
		if err != nil {
			return err
		}
		if d.Status != "frozen" && d.Status != "signed" {
			return ErrState
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if official != "" {
			if !now.Before(s.roundTime(d)) {
				return ErrTooLate
			}
			if _, err := signoff(ctx, q, "void", official, d, now); err != nil {
				return err
			}
		}
		if err := q.VoidDraw(ctx, store.VoidDrawParams{Reason: &reason, At: now, ID: d.ID}); err != nil {
			return err
		}
		if rerun, err = q.InsertRerun(ctx, store.InsertRerunParams{DrawType: d.DrawType, PeriodStart: d.PeriodStart, PeriodEnd: d.PeriodEnd,
			RerunOf: &d.ID, At: now}); err != nil {
			return err
		}
		r, err := q.LockDraw(ctx, rerun)
		if err != nil {
			return err
		}
		return s.freezeTx(ctx, tx, r, now)
	})
	if err != nil {
		return Draw{}, err
	}
	slog.WarnContext(ctx, "draw voided and re-run", "draw", drawID, "rerun", rerun, "reason", reason, "official", official)
	return s.Get(ctx, rerun)
}

// Certificate is the public record of a conducted draw: everything needed to recompute the winners.
type Certificate struct {
	DrawID        string         `json:"draw_id"`
	DrawType      string         `json:"draw_type"`
	PeriodStart   string         `json:"period_start"`
	PeriodEnd     string         `json:"period_end"`
	Entries       int            `json:"entries"`
	EntryListHash string         `json:"entry_list_hash"`
	SeedCommit    string         `json:"seed_commit"`
	RevealedSeed  string         `json:"revealed_seed"`
	Beacon        drand.Beacon   `json:"beacon"`
	ChainHash     string         `json:"drand_chain_hash"`
	StakesKES     int64          `json:"stakes_kes"`
	PoolShare     string         `json:"pool_share"`
	PoolKES       int64          `json:"pool_kes"`
	Rounding      string         `json:"rounding"`
	Prizes        []CertifiedWin `json:"prizes"`
	Algorithm     string         `json:"algorithm"`
}

// CertifiedWin is one awarded rank.
type CertifiedWin struct {
	Rank      int    `json:"rank"`
	Share     string `json:"share"`
	AmountKES int64  `json:"amount_kes"`
	EntryNo   int    `json:"entry_no"`
	UserID    string `json:"user_id"`
}

const algorithm = "key = SHA-256(drand randomness || company seed || entry list hash || draw id); pick i = first 64-bit " +
	"big-endian word w of HMAC-SHA256(key, \"pick:i:round\") with w < 2^64 - (2^64 mod n), entry w mod n + 1; " +
	"an entry of a player who already won is skipped"

// Pick returns the i-th pick (1-based entry number) among n entries.
func Pick(key []byte, i, n int) int {
	nn := uint64(n) //nolint:gosec // n is a positive entry count
	rem := -nn % nn // 2^64 mod n, computed as (2^64 - n) mod n; words at or above 2^64 - rem are rejected
	for round := 0; ; round++ {
		m := hmac.New(sha256.New, key)
		_, _ = m.Write([]byte("pick:" + strconv.Itoa(i) + ":" + strconv.Itoa(round)))
		sum := m.Sum(nil)
		for j := 0; j < len(sum); j += 8 {
			if w := binary.BigEndian.Uint64(sum[j:]); rem == 0 || w < -rem {
				return int(w%nn) + 1 //nolint:gosec // w % nn < n
			}
		}
	}
}

// Key derives the draw's random key.
func Key(randomness, seed []byte, entryListHash, drawID string) []byte {
	h := sha256.New()
	h.Write(randomness)
	h.Write(seed)
	elh, _ := hex.DecodeString(entryListHash)
	h.Write(elh)
	h.Write([]byte(drawID))
	return h.Sum(nil)
}

// Conduct fetches the draw's verified beacon, reveals the company seed and picks the winners.
func (s *Service) Conduct(ctx context.Context, drawID string) (Draw, error) {
	d, err := store.New(s.pool).GetDraw(ctx, drawID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Draw{}, ErrNotFound
	} else if err != nil {
		return Draw{}, err
	}
	if d.Status != "signed" || d.BeaconRound == nil {
		return Draw{}, ErrState
	}
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return Draw{}, err
	}
	// the network call happens before the transaction (never inside one)
	b, err := s.beacon.Fetch(ctx, now, uint64(*d.BeaconRound)) //nolint:gosec // rounds are positive
	if err != nil {
		return Draw{}, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		d, err := lock(ctx, q, drawID)
		if err != nil {
			return err
		}
		if d.Status != "signed" {
			return ErrState
		}
		seed, err := s.keys.Decrypt(d.SeedEnc, seedAAD(d.ID))
		if err != nil {
			return err
		}
		if c := sha256.Sum256(seed); hex.EncodeToString(c[:]) != deref(d.SeedCommit) {
			return fmt.Errorf("draws: draw %s seed does not match its commitment", d.ID)
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		cert, err := s.certify(ctx, q, d, seed, b, now)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(cert)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		return q.Conduct(ctx, store.ConductParams{RevealedSeed: strPtr(cert.RevealedSeed), BeaconValue: strPtr(b.Randomness),
			CertificateJson: raw, CertificateHash: strPtr(hex.EncodeToString(sum[:])), At: now, ID: d.ID})
	})
	if err != nil {
		return Draw{}, err
	}
	return s.Get(ctx, drawID)
}

type rankShare struct {
	Rank  int    `json:"rank"`
	Share string `json:"share"`
}

// certify picks the winners, records them and returns the certificate.
func (s *Service) certify(ctx context.Context, q *store.Queries, d store.DrawsDraw, seed []byte, b drand.Beacon, now time.Time) (Certificate, error) {
	entries, err := q.Entries(ctx, d.ID)
	if err != nil {
		return Certificate{}, err
	}
	ps, err := q.PrizeStructureFor(ctx, store.PrizeStructureForParams{DrawType: d.DrawType, OnDate: d.PeriodEnd})
	if errors.Is(err, pgx.ErrNoRows) {
		return Certificate{}, ErrNoPrizeVersion
	} else if err != nil {
		return Certificate{}, err
	}
	stakes, err := q.StakesIn(ctx, store.StakesInParams{PeriodStart: d.PeriodStart, PeriodEnd: d.PeriodEnd})
	if err != nil {
		return Certificate{}, err
	}
	mode := ledger.Rounding(ps.Rounding)
	poolRate, err := ledger.ParseRate(ps.PoolShare)
	if err != nil {
		return Certificate{}, err
	}
	pool, err := ledger.Apply(stakes, poolRate, mode)
	if err != nil {
		return Certificate{}, err
	}
	var ranks []rankShare
	if err := json.Unmarshal(ps.Prizes, &ranks); err != nil {
		return Certificate{}, err
	}
	randomness, err := hex.DecodeString(b.Randomness)
	if err != nil {
		return Certificate{}, err
	}
	cert := Certificate{DrawID: d.ID, DrawType: d.DrawType, PeriodStart: d.PeriodStart.Format(time.DateOnly), PeriodEnd: d.PeriodEnd.Format(time.DateOnly),
		Entries: len(entries), EntryListHash: deref(d.EntryListHash), SeedCommit: deref(d.SeedCommit), RevealedSeed: hex.EncodeToString(seed),
		Beacon: b, ChainHash: s.beacon.Chain().Hash, StakesKES: stakes, PoolShare: ps.PoolShare, PoolKES: pool, Rounding: ps.Rounding,
		Prizes: []CertifiedWin{}, Algorithm: algorithm}
	players := map[string]bool{}
	for _, e := range entries {
		players[e.UserID] = true
	}
	key := Key(randomness, seed, cert.EntryListHash, d.ID)
	won := map[string]bool{}
	for i, r := 0, 0; r < len(ranks) && len(won) < len(players); i++ {
		share, err := ledger.ParseRate(ranks[r].Share)
		if err != nil {
			return Certificate{}, err
		}
		amount, err := ledger.Apply(pool, share, mode)
		if err != nil {
			return Certificate{}, err
		}
		if amount <= 0 { // nothing left to award at this rank or below
			break
		}
		e := entries[Pick(key, i, len(entries))-1]
		if won[e.UserID] {
			continue
		}
		won[e.UserID] = true
		if err := q.InsertResult(ctx, store.InsertResultParams{DrawID: d.ID, EntryID: e.ID, PrizeRank: int32(ranks[r].Rank), AmountKes: amount, CreatedAt: now}); err != nil { //nolint:gosec // ranks are small
			return Certificate{}, err
		}
		cert.Prizes = append(cert.Prizes, CertifiedWin{Rank: ranks[r].Rank, Share: ranks[r].Share, AmountKES: amount, EntryNo: int(e.EntryNo), UserID: e.UserID})
		r++
	}
	return cert, nil
}

// Approve records an official's approval of a conducted draw. The second different official approves it and the
// pool moves from stakes revenue to the draw type's prize pool.
func (s *Service) Approve(ctx context.Context, drawID, official string) (Draw, error) {
	if strings.TrimSpace(official) == "" {
		return Draw{}, ErrOfficial
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		d, err := lock(ctx, q, drawID)
		if err != nil {
			return err
		}
		if d.Status != "conducted" {
			return ErrState
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		n, err := signoff(ctx, q, "approve", official, d, now)
		if err != nil || n < Officials {
			return err
		}
		var cert Certificate
		if err := json.Unmarshal(d.CertificateJson, &cert); err != nil {
			return err
		}
		if cert.PoolKES > 0 {
			p, err := ledger.PrizePoolFunding(d.ID, d.DrawType, cert.PoolKES)
			if err != nil {
				return err
			}
			if _, err := s.ledger.Post(ctx, tx, p.Journal); err != nil {
				return err
			}
		}
		return q.SetStatus(ctx, store.SetStatusParams{Status: "approved", At: now, ID: d.ID})
	})
	if err != nil {
		return Draw{}, err
	}
	return s.Get(ctx, drawID)
}

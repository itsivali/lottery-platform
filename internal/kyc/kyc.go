// Package kyc verifies a player's national ID against the population register through a pluggable provider.
// One verified national ID is one account; under-18s are refused; mismatches go to manual review.
package kyc

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/kyc/store"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

// Status is the account's KYC state (identity.users.kyc_status).
type Status string

// KYC states.
const (
	None         Status = "none"
	Pending      Status = "pending"
	Verified     Status = "verified"
	ManualReview Status = "manual_review"
	Rejected     Status = "rejected"
)

// Errors.
var (
	ErrInvalidInput        = errors.New("kyc: invalid input")
	ErrUnderage            = errors.New("kyc: players must be 18 or older")
	ErrIDAlreadyRegistered = errors.New("kyc: this national ID is already registered")
	ErrAlreadyVerified     = errors.New("kyc: already verified")
	ErrInReview            = errors.New("kyc: a submission is already awaiting review")
	ErrProviderUnavailable = errors.New("kyc: verification service unavailable")
	ErrNothingToReview     = errors.New("kyc: no submission awaiting review")
)

// Request is what the provider checks.
type Request struct {
	IDNumber    string
	FullName    string
	DateOfBirth time.Time
}

// Result is the provider's answer.
type Result struct {
	Found     bool
	NameMatch bool
	DOBMatch  bool
	Ref       string
}

// Provider is the national-registry adapter.
type Provider interface {
	Name() string
	Verify(ctx context.Context, r Request) (Result, error)
}

// Submission is what the player enters. DateOfBirth is YYYY-MM-DD.
type Submission struct {
	IDNumber    string
	FullName    string
	DateOfBirth string
}

var (
	idPattern   = regexp.MustCompile(`^[0-9]{7,9}$`)
	namePattern = regexp.MustCompile(`^\p{L}[\p{L} .'-]{1,99}$`)
)

// Service runs KYC.
type Service struct {
	pool  *pgxpool.Pool
	clk   clock.Clock
	prov  Provider
	kr    *crypto.Keyring
	idKey []byte
	audit *audit.Writer
}

// NewService returns a Service. idKey keys the lookup hash that enforces one account per national ID.
func NewService(pool *pgxpool.Pool, clk clock.Clock, prov Provider, kr *crypto.Keyring, idKey []byte, a *audit.Writer) *Service {
	return &Service{pool: pool, clk: clk, prov: prov, kr: kr, idKey: idKey, audit: a}
}

func (s *Service) idHash(id string) []byte { return crypto.MAC(s.idKey, "national-id", id) }

func age(dob, today time.Time) int {
	y := today.Year() - dob.Year()
	if today.Month() < dob.Month() || (today.Month() == dob.Month() && today.Day() < dob.Day()) {
		y--
	}
	return y
}

// Status returns the account's KYC state.
func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	st, err := store.New(s.pool).GetUserKYCStatus(ctx, userID)
	return Status(st), err
}

// Submit validates and verifies a player's identity details.
func (s *Service) Submit(ctx context.Context, userID string, in Submission) (Status, error) {
	name := strings.Join(strings.Fields(in.FullName), " ")
	if !idPattern.MatchString(in.IDNumber) || utf8.RuneCountInString(name) < 2 || !namePattern.MatchString(name) {
		return "", ErrInvalidInput
	}
	dob, err := time.Parse(time.DateOnly, in.DateOfBirth)
	if err != nil || dob.Year() < 1900 {
		return "", ErrInvalidInput
	}
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return "", err
	}
	today := clock.InNairobi(now)
	if dob.After(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) {
		return "", ErrInvalidInput
	}
	if age(dob, time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) < 18 {
		return "", ErrUnderage
	}

	q := store.New(s.pool)
	cur, err := q.GetUserKYCStatus(ctx, userID)
	if err != nil {
		return "", err
	}
	switch Status(cur) {
	case Verified:
		return "", ErrAlreadyVerified
	case ManualReview, Pending:
		return "", ErrInReview
	}
	hash := s.idHash(in.IDNumber)
	if _, err := q.IDHashOwner(ctx, store.IDHashOwnerParams{NationalIDHash: hash, ID: userID}); err == nil {
		return "", ErrIDAlreadyRegistered
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	res, perr := s.prov.Verify(ctx, Request{IDNumber: in.IDNumber, FullName: name, DateOfBirth: dob})
	rec, err := s.encrypt(in.IDNumber, name, in.DateOfBirth)
	if err != nil {
		return "", err
	}
	rec.hash = hash
	rec.adult = true
	rec.provider = s.prov.Name()

	var outcome Status
	var outErr error
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.LockUserKYC(ctx, userID); err != nil {
			return err
		}
		if perr != nil {
			rec.status = "error"
			_, err := insertCheck(ctx, q, userID, nil, rec, now)
			outErr = ErrProviderUnavailable
			return err
		}
		rec.providerRef = res.Ref
		rec.nameMatch, rec.dobMatch = &res.NameMatch, &res.DOBMatch
		var hashToSet []byte
		if res.Found && res.NameMatch && res.DOBMatch {
			outcome, rec.status, hashToSet = Verified, "verified", hash
		} else {
			outcome, rec.status = ManualReview, "manual_review"
		}
		if _, err := insertCheck(ctx, q, userID, nil, rec, now); err != nil {
			return err
		}
		if err := q.SetUserKYC(ctx, store.SetUserKYCParams{KycStatus: string(outcome), NationalIDHash: hashToSet, At: now, ID: userID}); err != nil {
			return mapUnique(err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, outErr
}

type checkRecord struct {
	idEnc, nameEnc, dobEnc []byte
	hash                   []byte
	nameMatch, dobMatch    *bool
	adult                  bool
	status, provider       string
	providerRef            string
	reviewer, reason       string
}

func (s *Service) encrypt(id, name, dob string) (checkRecord, error) {
	var r checkRecord
	var err error
	if r.idEnc, err = s.kr.Encrypt([]byte(id), []byte("kyc.id_number")); err != nil {
		return r, err
	}
	if r.nameEnc, err = s.kr.Encrypt([]byte(name), []byte("kyc.full_name")); err != nil {
		return r, err
	}
	r.dobEnc, err = s.kr.Encrypt([]byte(dob), []byte("kyc.dob"))
	return r, err
}

func insertCheck(ctx context.Context, q *store.Queries, userID string, supersedes *string, r checkRecord, at time.Time) (string, error) {
	p := store.InsertKYCCheckParams{
		UserID: userID, SupersedesID: supersedes, IDNumberEnc: r.idEnc, IDNumberHash: r.hash, FullNameEnc: r.nameEnc,
		DateOfBirthEnc: r.dobEnc, IsAdult: pgtype.Bool{Bool: r.adult, Valid: true}, Status: r.status, Provider: r.provider, CreatedAt: at,
	}
	if r.nameMatch != nil {
		p.NameMatch = pgtype.Bool{Bool: *r.nameMatch, Valid: true}
		p.DobMatch = pgtype.Bool{Bool: *r.dobMatch, Valid: true}
	}
	if r.providerRef != "" {
		p.ProviderRef = &r.providerRef
	}
	if r.reviewer != "" {
		p.ReviewerID, p.ReviewerReason = &r.reviewer, &r.reason
	}
	return q.InsertKYCCheck(ctx, p)
}

func mapUnique(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrIDAlreadyRegistered
	}
	return err
}

// Review records a staff decision on a submission awaiting manual review (a new row supersedes the old one).
func (s *Service) Review(ctx context.Context, userID, reviewer string, approve bool, reason string) (Status, error) {
	if reviewer == "" || strings.TrimSpace(reason) == "" {
		return "", errors.New("kyc: a named reviewer and a reason are required")
	}
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return "", err
	}
	var outcome Status
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		cur, err := q.LockUserKYC(ctx, userID)
		if err != nil {
			return err
		}
		if Status(cur.KycStatus) != ManualReview {
			return ErrNothingToReview
		}
		last, err := q.LatestKYCCheck(ctx, userID)
		if err != nil {
			return err
		}
		rec := checkRecord{
			idEnc: last.IDNumberEnc, nameEnc: last.FullNameEnc, dobEnc: last.DateOfBirthEnc, hash: last.IDNumberHash,
			adult: true, provider: last.Provider, reviewer: reviewer, reason: reason,
		}
		var hashToSet []byte
		if approve {
			outcome, rec.status, hashToSet = Verified, "verified", last.IDNumberHash
		} else {
			outcome, rec.status = Rejected, "rejected"
		}
		if _, err := insertCheck(ctx, q, userID, &last.ID, rec, now); err != nil {
			return err
		}
		if err := q.SetUserKYC(ctx, store.SetUserKYCParams{KycStatus: string(outcome), NationalIDHash: hashToSet, At: now, ID: userID}); err != nil {
			return mapUnique(err)
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Staff, ActorID: reviewer, Action: "kyc.review", EntityType: "user", EntityID: userID,
			Details: map[string]any{"decision": string(outcome), "reason": reason}})
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// Package ledger holds the double-entry model: journals, entries and the pure posting rules that turn a financial
// event plus the tax rule in force into a balanced journal. Persistence is in store.go; nothing here touches I/O.
package ledger

import (
	"errors"
	"fmt"
)

// Direction of a ledger entry.
type Direction string

// Entry directions.
const (
	Debit  Direction = "D"
	Credit Direction = "C"
)

// Chart-of-accounts codes (seeded by migrations 00004 and 00009).
const (
	WalletLiability = "WALLET_LIABILITY"
	MpesaClearing   = "MPESA_CLEARING"
	B2CFloat        = "B2C_FLOAT"
	StakesRevenue   = "STAKES_REVENUE"
	ExciseExpense   = "EXCISE_EXPENSE"
	ExcisePayable   = "EXCISE_PAYABLE"
	TaxPayable      = "TAX_PAYABLE"
	Suspense        = "SUSPENSE"
	PlayerDebt      = "PLAYER_DEBT"
)

var prizePools = map[string]string{
	"daily": "PRIZE_POOL_DAILY", "weekly": "PRIZE_POOL_WEEKLY",
	"monthly": "PRIZE_POOL_MONTHLY", "quarterly": "PRIZE_POOL_QUARTERLY",
}

var knownAccounts = map[string]bool{
	WalletLiability: true, MpesaClearing: true, B2CFloat: true, StakesRevenue: true, ExciseExpense: true,
	ExcisePayable: true, TaxPayable: true, Suspense: true, PlayerDebt: true, "REFUNDS": true,
	"PRIZE_POOL_DAILY": true, "PRIZE_POOL_WEEKLY": true, "PRIZE_POOL_MONTHLY": true, "PRIZE_POOL_QUARTERLY": true,
}

// MaxAmountKES bounds every amount so rate multiplication can never overflow int64 (1e12 * 1e5 < 9.2e18).
const MaxAmountKES int64 = 1_000_000_000_000

// Entry is one side of a posting. UserID is set on player-wallet postings so each wallet reconciles on its own.
type Entry struct {
	Account   string
	Dir       Direction
	AmountKES int64
	UserID    string
}

// Journal is one financial event. (Kind, RefType, RefID) is unique: posting the same event twice is impossible.
type Journal struct {
	Kind, RefType, RefID string
	Entries              []Entry
}

// Validate checks the journal is well formed and balanced.
func (j Journal) Validate() error {
	if j.Kind == "" || j.RefType == "" || j.RefID == "" {
		return errors.New("ledger: journal needs kind, reference type and reference id")
	}
	if len(j.Entries) < 2 {
		return fmt.Errorf("ledger: journal has %d entries, need at least 2", len(j.Entries))
	}
	var net int64
	for _, e := range j.Entries {
		if !knownAccounts[e.Account] {
			return fmt.Errorf("ledger: unknown account %q", e.Account)
		}
		if e.AmountKES <= 0 || e.AmountKES > MaxAmountKES {
			return fmt.Errorf("ledger: entry amount %d out of range", e.AmountKES)
		}
		switch e.Dir {
		case Debit:
			net += e.AmountKES
		case Credit:
			net -= e.AmountKES
		default:
			return fmt.Errorf("ledger: bad direction %q", e.Dir)
		}
	}
	if net != 0 {
		return fmt.Errorf("ledger: journal does not balance (debits - credits = %d)", net)
	}
	return nil
}

// WalletTx is one row the wallet module appends to wallet_transactions for the posting. Signed amount.
type WalletTx struct {
	Kind      string
	AmountKES int64
}

// Posting couples a journal with the wallet transactions it implies. The sum of WalletTxs always equals the net
// credit to the player's WALLET_LIABILITY entries, which is what keeps wallet and ledger reconciled.
type Posting struct {
	UserID    string // empty when the event does not touch a player wallet
	Journal   Journal
	WalletTxs []WalletTx
}

// WalletDelta is the net movement of the player's wallet.
func (p Posting) WalletDelta() int64 {
	var s int64
	for _, w := range p.WalletTxs {
		s += w.AmountKES
	}
	return s
}

func positive(amount int64) error {
	if amount <= 0 || amount > MaxAmountKES {
		return fmt.Errorf("ledger: amount %d out of range", amount)
	}
	return nil
}

// build drops zero-amount entries (a zero tax, say) and validates the result.
func build(kind, refType, ref, user string, walletTxs []WalletTx, entries ...Entry) (Posting, error) {
	kept := entries[:0:0]
	for _, e := range entries {
		if e.AmountKES != 0 {
			kept = append(kept, e)
		}
	}
	var txs []WalletTx
	for _, w := range walletTxs {
		if w.AmountKES != 0 {
			txs = append(txs, w)
		}
	}
	p := Posting{UserID: user, Journal: Journal{Kind: kind, RefType: refType, RefID: ref, Entries: kept}, WalletTxs: txs}
	if err := p.Journal.Validate(); err != nil {
		return Posting{}, err
	}
	return p, nil
}

func d(account string, amt int64, user string) Entry {
	return Entry{Account: account, Dir: Debit, AmountKES: amt, UserID: user}
}

func c(account string, amt int64, user string) Entry {
	return Entry{Account: account, Dir: Credit, AmountKES: amt, UserID: user}
}

// Deposit posts a verified top-up of the confirmed gross amount. Excise per the rule in force is either deducted
// from what the player receives (shown as its own wallet line) or absorbed by the operator.
func Deposit(paymentID, user string, gross int64, excise TaxRule, deductFromCredit bool) (Posting, error) {
	if err := positive(gross); err != nil {
		return Posting{}, err
	}
	e, err := Apply(gross, excise.Rate, excise.Rounding)
	if err != nil {
		return Posting{}, err
	}
	entries := []Entry{d(MpesaClearing, gross, ""), c(WalletLiability, gross, user)}
	txs := []WalletTx{{Kind: "deposit", AmountKES: gross}}
	if deductFromCredit {
		entries = append(entries, d(WalletLiability, e, user), c(ExcisePayable, e, ""))
		txs = append(txs, WalletTx{Kind: "deposit_excise", AmountKES: -e})
	} else {
		entries = append(entries, d(ExciseExpense, e, ""), c(ExcisePayable, e, ""))
	}
	return build("deposit", "payment", paymentID, user, txs, entries...)
}

// Stake posts the KES price of a play.
func Stake(ticketID, user string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("stake", "ticket", ticketID, user, []WalletTx{{Kind: "stake", AmountKES: -amount}},
		d(WalletLiability, amount, user), c(StakesRevenue, amount, ""))
}

// StakeRefund returns a stake to the wallet.
func StakeRefund(ticketID, user string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("stake_refund", "ticket", ticketID, user, []WalletTx{{Kind: "refund", AmountKES: amount}},
		d(StakesRevenue, amount, ""), c(WalletLiability, amount, user))
}

// Withdrawal debits the wallet and moves cash out of the B2C float, net of tax per the rule in force.
func Withdrawal(payoutID, user string, amount int64, tax TaxRule) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	t, err := Apply(amount, tax.Rate, tax.Rounding)
	if err != nil {
		return Posting{}, err
	}
	return build("withdrawal", "payout", payoutID, user, []WalletTx{{Kind: "withdrawal", AmountKES: -amount}},
		d(WalletLiability, amount, user), c(B2CFloat, amount-t, ""), c(TaxPayable, t, ""))
}

// WithdrawalFailed reverses a withdrawal whose B2C payment finally failed.
func WithdrawalFailed(payoutID, user string, amount int64, tax TaxRule) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	t, err := Apply(amount, tax.Rate, tax.Rounding)
	if err != nil {
		return Posting{}, err
	}
	return build("withdrawal_failed", "payout", payoutID, user, []WalletTx{{Kind: "refund", AmountKES: amount}},
		d(B2CFloat, amount-t, ""), d(TaxPayable, t, ""), c(WalletLiability, amount, user))
}

// DepositReversal claws back a credited top-up that M-Pesa reversed: M-Pesa takes back the gross, the player gives
// back what they received (gross minus the excise deducted from the credit), and the excise already owed to KRA
// becomes the operator's cost (whether excise is refundable on a reversed deposit is an open tax question). The wallet
// gives back what it can cover; any shortfall becomes player debt, booked against the player so it can be totalled
// and recovered (DebtRecovery); play and withdrawals are suspended by the caller while it is owed.
func DepositReversal(paymentID, user string, gross, exciseDeducted, walletBalance int64) (Posting, error) {
	if err := positive(gross); err != nil {
		return Posting{}, err
	}
	if exciseDeducted < 0 || exciseDeducted > gross {
		return Posting{}, errors.New("ledger: excise out of range")
	}
	if walletBalance < 0 {
		return Posting{}, errors.New("ledger: negative wallet balance")
	}
	received := gross - exciseDeducted
	taken := min(walletBalance, received)
	shortfall := received - taken
	return build("deposit_reversal", "payment", paymentID, user, []WalletTx{{Kind: "reversal", AmountKES: -taken}},
		d(WalletLiability, taken, user), d(PlayerDebt, shortfall, user), d(ExciseExpense, exciseDeducted, ""),
		c(MpesaClearing, gross, ""))
}

// PrizePayout pays a draw prize out of the prize pool via B2C; it does not pass through the wallet.
func PrizePayout(payoutID, drawType string, gross int64, tax TaxRule) (Posting, error) {
	pool, ok := prizePools[drawType]
	if !ok {
		return Posting{}, fmt.Errorf("ledger: unknown draw type %q", drawType)
	}
	if err := positive(gross); err != nil {
		return Posting{}, err
	}
	t, err := Apply(gross, tax.Rate, tax.Rounding)
	if err != nil {
		return Posting{}, err
	}
	return build("prize_payout", "payout", payoutID, "", nil,
		d(pool, gross, ""), c(B2CFloat, gross-t, ""), c(TaxPayable, t, ""))
}

// DebtRecovery takes back, from a later deposit, money a player owes after a reversal. It is keyed on the payment
// whose deposit funds it, so each deposit recovers at most once.
func DebtRecovery(paymentID, user string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("debt_recovery", "payment", paymentID, user, []WalletTx{{Kind: "debt_recovery", AmountKES: -amount}},
		d(WalletLiability, amount, user), c(PlayerDebt, amount, user))
}

// C2BSuspense parks a verified paybill payment that matches no eligible account: M-Pesa holds the money
// (MPESA_CLEARING) and the operator owes it to someone not yet known (SUSPENSE). Keyed on the suspense item.
func C2BSuspense(itemID string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("c2b_suspense", "c2b_unmatched", itemID, "", nil, d(MpesaClearing, amount, ""), c(Suspense, amount, ""))
}

// C2BSuspenseRelease takes a suspense item out of suspense so it can be credited as an ordinary deposit (which debits
// MPESA_CLEARING again), leaving each M-Pesa shilling counted once.
func C2BSuspenseRelease(itemID string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("c2b_suspense_release", "c2b_unmatched", itemID, "", nil, d(Suspense, amount, ""), c(MpesaClearing, amount, ""))
}

// C2BSuspenseRefund records that a suspense item was sent back to the payer through M-Pesa.
func C2BSuspenseRefund(itemID string, amount int64) (Posting, error) {
	if err := positive(amount); err != nil {
		return Posting{}, err
	}
	return build("c2b_suspense_refund", "c2b_unmatched", itemID, "", nil, d(Suspense, amount, ""), c(MpesaClearing, amount, ""))
}

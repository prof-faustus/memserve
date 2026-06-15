// Package channel implements MemServe's pay-per-use BSV payment channel
// (DESIGN.md §10): a native unidirectional micropayment channel, PREPAY-THEN-SERVE,
// per-shard, one signature per access.
//
// Flow (the server can never lose even one access):
//
//  1. Open/fund: client locks a Deposit into a funding output; the server has
//     counter-signed a refund tx with nLockTime = RefundLockTime (the client's
//     safety net — funds are never stuck).
//  2. Prepay, then serve: to get access k the client signs a commitment paying the
//     cumulative fee through access k; the server verifies that signature and the
//     increment BEFORE serving. Payment always leads service, so a client who stops
//     only forfeits its own prepayment — it can cheat only itself.
//  3. Release/settle: the server broadcasts the best commitment when it has sold N
//     accesses and/or at a settle-before time x' < x (both configurable). One on-chain
//     tx settles all accesses. The settlement fee (mining fee + settle cost) is built
//     in, so abandoning a channel cannot dodge it.
//
// Cryptography is real: commitments are secp256k1 ECDSA signatures (RFC 6979, low-S)
// over a canonical message binding the funding outpoint, the server payee, the channel
// id and the cumulative amount. On-chain tx serialization is modeled at the field
// level; the security-relevant authorization (signature validity, low-S, cumulative
// monotonicity, deposit bound, prepay ordering, settlement triggers) is fully real and
// tested. BSV only — secp256k1, nLockTime per BSV consensus.
package channel

import (
	"errors"
	"sync"

	"memserve/bsvtx"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/store"
)

// QueryType is the priced operation (DESIGN.md §10.5: pricing is configurable).
type QueryType uint8

const (
	QSeen QueryType = iota
	QMined
	QMerklePath
	QUTXO
	numQueryTypes
)

// FeeMode selects how the built-in settlement fee is collected.
type FeeMode uint8

const (
	// FeeUpfront charges the full settlement fee on the first access (works with any
	// N, including time-only channels; one access already pays the fee).
	FeeUpfront FeeMode = iota
	// FeeAmortized spreads the settlement fee evenly across the N accesses (requires N>0).
	FeeAmortized
)

// Pricing is the configurable price structure: flat per access, or metered per query
// type, plus the built-in settlement fee.
type Pricing struct {
	Flat      bool                  // true => FlatPrice for every query; false => PerType
	FlatPrice uint64                // satoshis per access when Flat
	PerType   [numQueryTypes]uint64 // satoshis per query type when metered
	SettleFee uint64                // total settlement fee (incl. on-chain mining fee)
	FeeMode   FeeMode
}

func (p Pricing) service(q QueryType) uint64 {
	if p.Flat {
		return p.FlatPrice
	}
	if int(q) < len(p.PerType) {
		return p.PerType[q]
	}
	return 0
}

func ceilDiv(a, b uint64) uint64 {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// Params bind a commitment to the channel's funding 2-of-2 output and both parties'
// keys. A commitment is a signature over the REAL BSV commitment-transaction FORKID
// sighash built from these (bsvtx), so it cannot be replayed against a different funding
// output, payee or amount. Client and server agree on Params at open.
type Params struct {
	ChannelID    store.Hash
	FundingTxID  store.Hash // the 2-of-2 funding tx id (internal byte order)
	FundingVout  uint32
	ClientPub    []byte // compressed (33) — also the verifying key
	ServerPub    []byte // compressed (33) — the payee
	FundingValue uint64 // satoshis locked in the 2-of-2 output
	Fee          uint64 // miner fee reserved for the settling tx
}

// channelTx reconstructs the on-chain channel-tx builder from Params.
func (p Params) channelTx() bsvtx.ChannelTx {
	var fund [32]byte
	copy(fund[:], p.FundingTxID[:])
	return bsvtx.ChannelTx{
		FundingOut:   bsvtx.OutPoint{Hash: fund, Index: p.FundingVout},
		FundingValue: p.FundingValue,
		ClientPub:    p.ClientPub,
		ServerPub:    p.ServerPub,
		Fee:          p.Fee,
	}
}

// Commitment is one prepay authorization: the client's signature over the cumulative
// amount it authorizes the server to take from the funding output.
type Commitment struct {
	CumAmount uint64
	Sig       *crypto.Signature
}

// Config opens a channel (server-side state).
type Config struct {
	Params         Params
	Deposit        uint64
	ClientPub      *crypto.PublicKey
	Pricing        Pricing
	N              uint64 // settle after N accesses (0 = no count trigger; needs a time trigger)
	RefundLockTime uint32 // client refund nLockTime x
	SettleBefore   uint32 // server settle deadline x' (must be < x when both set)
}

// Errors.
var (
	ErrBadSig         = errors.New("channel: invalid or non-canonical commitment signature")
	ErrUnderpaid      = errors.New("channel: commitment does not cover the next access")
	ErrExceedsDeposit = errors.New("channel: cumulative amount exceeds the deposit")
	ErrClosed         = errors.New("channel: already settled/closed")
	ErrConfig         = errors.New("channel: invalid configuration")
)

// Channel is server-side channel state.
type Channel struct {
	cfg Config

	mu       sync.Mutex
	accesses uint64
	cumPaid  uint64
	best     *Commitment
	closed   bool
}

// Open validates the configuration and returns a ready channel.
func Open(cfg Config) (*Channel, error) {
	if cfg.ClientPub == nil || cfg.Deposit == 0 {
		return nil, ErrConfig
	}
	if cfg.Pricing.FeeMode == FeeAmortized && cfg.N == 0 {
		return nil, ErrConfig // cannot amortize over zero accesses
	}
	if cfg.N == 0 && cfg.SettleBefore == 0 {
		return nil, ErrConfig // need at least one settlement trigger (count or time)
	}
	if cfg.RefundLockTime != 0 && cfg.SettleBefore != 0 && cfg.SettleBefore >= cfg.RefundLockTime {
		return nil, ErrConfig // must settle before the client's refund matures
	}
	// Default the on-chain funded value to the channel capacity, and bind the client
	// verifying key into Params if not already set.
	if cfg.Params.FundingValue == 0 {
		cfg.Params.FundingValue = cfg.Deposit
	}
	if len(cfg.Params.ClientPub) == 0 && cfg.ClientPub != nil {
		cfg.Params.ClientPub = cfg.ClientPub.SerializeCompressed()
	}
	return &Channel{cfg: cfg}, nil
}

// feeComponent returns the settlement-fee portion charged on the given (1-based) access.
func (ch *Channel) feeComponent(accessNo uint64) uint64 {
	switch ch.cfg.Pricing.FeeMode {
	case FeeUpfront:
		if accessNo == 1 {
			return ch.cfg.Pricing.SettleFee
		}
		return 0
	case FeeAmortized:
		return ceilDiv(ch.cfg.Pricing.SettleFee, ch.cfg.N)
	default:
		return 0
	}
}

// Quote returns the cumulative amount the client must sign to obtain the next access
// of type q. The client signs exactly this (or more) and sends it before being served.
func (ch *Channel) Quote(q QueryType) uint64 {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.cumPaid + ch.cfg.Pricing.service(q) + ch.feeComponent(ch.accesses+1)
}

// Authorize verifies a client commitment for the next access of type q and, on
// success, advances channel state (the access is now prepaid). It is called BEFORE the
// server serves the answer. Returns an error and leaves state unchanged on any failure.
func (ch *Channel) Authorize(c *Commitment, q QueryType) error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return ErrClosed
	}
	if c == nil || c.Sig == nil {
		return ErrBadSig
	}
	required := ch.cumPaid + ch.cfg.Pricing.service(q) + ch.feeComponent(ch.accesses+1)
	if c.CumAmount < required {
		return ErrUnderpaid
	}
	if c.CumAmount > ch.cfg.Deposit {
		return ErrExceedsDeposit
	}
	// Verify the signature over the canonical commitment message.
	if !verify(ch.cfg.ClientPub, ch.cfg.Params, c.CumAmount, c.Sig) {
		return ErrBadSig
	}
	// Accept: this access is prepaid.
	ch.cumPaid = c.CumAmount
	ch.accesses++
	ch.best = c
	return nil
}

// Snapshot of channel progress.
type Snapshot struct {
	Accesses uint64
	CumPaid  uint64
	Deposit  uint64
	Closed   bool
}

// Snapshot returns current progress.
func (ch *Channel) Snapshot() Snapshot {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return Snapshot{Accesses: ch.accesses, CumPaid: ch.cumPaid, Deposit: ch.cfg.Deposit, Closed: ch.closed}
}

// ShouldSettle reports whether a settlement trigger has fired at chain time `now`
// (count: N accesses sold; time: settle-before reached).
func (ch *Channel) ShouldSettle(now uint32) (bool, string) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return false, "closed"
	}
	if ch.cfg.N > 0 && ch.accesses >= ch.cfg.N {
		return true, "count"
	}
	if ch.cfg.SettleBefore > 0 && now >= ch.cfg.SettleBefore {
		return true, "time"
	}
	return false, ""
}

// Settlement is the modeled on-chain settlement tx (field level).
type Settlement struct {
	ToServer  uint64 // amount the server collects (cumPaid, incl. collected settle fee)
	ToClient  uint64 // change returned to the client (Deposit - cumPaid)
	MiningFee uint64 // network fee paid out of the built-in settle fee
	NetServer uint64 // ToServer - MiningFee
	Accesses  uint64
}

// Settle closes the channel and returns the modeled settlement, collecting cumPaid.
// The built-in settle fee (already inside cumPaid) funds the mining fee, so the server
// is never out of pocket for settling. Idempotent: a second call returns ErrClosed.
func (ch *Channel) Settle() (Settlement, error) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return Settlement{}, ErrClosed
	}
	ch.closed = true
	mining := ch.cfg.Pricing.SettleFee
	if mining > ch.cumPaid {
		mining = ch.cumPaid
	}
	return Settlement{
		ToServer:  ch.cumPaid,
		ToClient:  ch.cfg.Deposit - ch.cumPaid,
		MiningFee: mining,
		NetServer: ch.cumPaid - mining,
		Accesses:  ch.accesses,
	}, nil
}

// Refund is the client's safety net: the counter-signed refund tx (nLockTime = x)
// returning the full deposit if the server never settles.
type Refund struct {
	LockTime uint32
	Amount   uint64
	ToClient bool
}

// Refund returns the refund terms.
func (ch *Channel) Refund() Refund {
	return Refund{LockTime: ch.cfg.RefundLockTime, Amount: ch.cfg.Deposit, ToClient: true}
}

// --- commitment signing/verification (over the REAL BSV commitment tx) -------

// sighash returns the FORKID sighash of the BSV commitment transaction that pays the
// server `cum` from the funding 2-of-2 output. This is the actual thing the client
// signs — a real Bitcoin signature hash, not a bespoke message.
func sighash(p Params, cum uint64) ([]byte, error) {
	return p.channelTx().CommitmentSighash(cum)
}

func verify(pub *crypto.PublicKey, p Params, cum uint64, sig *crypto.Signature) bool {
	if !sig.IsLowS() { // reject malleable (high-S) commitments
		return false
	}
	h, err := sighash(p, cum)
	if err != nil {
		return false
	}
	return crypto.Verify(pub, h, sig)
}

// SignCommitment is the CLIENT side: it signs the commitment-tx FORKID sighash that pays
// the server `cum`. Deterministic (RFC 6979) and low-S. Errors if cum+fee exceeds the
// funded value (the client cannot authorize spending more than is locked).
func SignCommitment(priv *crypto.PrivateKey, p Params, cum uint64) (*Commitment, error) {
	h, err := sighash(p, cum)
	if err != nil {
		return nil, err
	}
	sig, err := priv.Sign(h)
	if err != nil {
		return nil, err
	}
	return &Commitment{CumAmount: cum, Sig: sig}, nil
}

// SettlementTx builds the BROADCASTABLE settlement transaction: the best (largest) client
// commitment co-signed by the server, spending the funding 2-of-2 to pay the server
// cumPaid and return the change to the client. Call after a settlement trigger fires.
func (ch *Channel) SettlementTx(serverPriv *crypto.PrivateKey) (*bsvtx.Tx, error) {
	ch.mu.Lock()
	best := ch.best
	cum := ch.cumPaid
	ch.mu.Unlock()
	if best == nil {
		return nil, errors.New("channel: no commitment to settle")
	}
	c := ch.cfg.Params.channelTx()
	tx, err := c.CommitmentTx(cum)
	if err != nil {
		return nil, err
	}
	serverSig, err := bsvtx.SignInput(serverPriv, tx, 0, c.Redeem(), c.FundingValue, bsvtx.SighashAllFork)
	if err != nil {
		return nil, err
	}
	clientSig := append(derFromSig(best.Sig), byte(bsvtx.SighashAllFork))
	bsvtx.Finalize2of2(tx, clientSig, serverSig)
	return tx, nil
}

// RefundTxUnsigned returns the client's refund transaction (nLockTime = RefundLockTime),
// which both parties sign at open (the server pre-signs). Spendable only at/after x.
func (ch *Channel) RefundTxUnsigned() *bsvtx.Tx {
	return ch.cfg.Params.channelTx().RefundTx(ch.cfg.RefundLockTime)
}

// derFromSig converts an (r,s) signature to the DER form used inside a scriptSig.
func derFromSig(s *crypto.Signature) []byte { return bsvtx.DEREncode(s.R, s.S) }

// DeriveChannelID derives a channel id from the funding outpoint.
func DeriveChannelID(fundingTxID store.Hash, vout uint32) store.Hash {
	buf := append(append([]byte{}, fundingTxID[:]...), byte(vout), byte(vout>>8), byte(vout>>16), byte(vout>>24))
	return commitment.DoubleSHA256(buf)
}

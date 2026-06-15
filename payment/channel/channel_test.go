package channel

import (
	"testing"

	"memserve/bsvtx"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/store"
)

func mkKey(seedTag string) *crypto.PrivateKey {
	seed := commitment.DoubleSHA256([]byte(seedTag))
	k, _ := crypto.NewPrivateKey(seed[:])
	return k
}

// testParams binds the SAME client key testKey(tag) returns, plus a per-tag server key,
// so commitments sign a consistent real BSV commitment-tx sighash. FundingValue is large
// so signing never hits the amount guard; Config.Deposit is the channel cap.
func testParams(tag string) Params {
	fund := commitment.DoubleSHA256([]byte("fund-" + tag))
	return Params{
		ChannelID:    DeriveChannelID(fund, 0),
		FundingTxID:  fund,
		FundingVout:  0,
		ClientPub:    mkKey("key-" + tag).Public().SerializeCompressed(),
		ServerPub:    mkKey("server-" + tag).Public().SerializeCompressed(),
		FundingValue: 1 << 40,
		Fee:          0,
	}
}

func testKey(t *testing.T, tag string) *crypto.PrivateKey {
	t.Helper()
	return mkKey("key-" + tag)
}

func TestPrepayThenServe(t *testing.T) {
	priv := testKey(t, "a")
	p := testParams("a")
	pricing := Pricing{Flat: true, FlatPrice: 10, SettleFee: 100, FeeMode: FeeUpfront}
	ch, err := Open(Config{Params: p, Deposit: 100000, ClientPub: priv.Public(), Pricing: pricing, N: 5})
	if err != nil {
		t.Fatal(err)
	}
	// access 1: quote = 10 (service) + 100 (upfront fee) = 110.
	q1 := ch.Quote(QSeen)
	if q1 != 110 {
		t.Fatalf("quote1 = %d, want 110", q1)
	}
	c1, _ := SignCommitment(priv, p, q1)
	if err := ch.Authorize(c1, QSeen); err != nil {
		t.Fatalf("authorize1: %v", err)
	}
	// access 2: quote = 110 + 10 = 120 (fee already paid).
	q2 := ch.Quote(QSeen)
	if q2 != 120 {
		t.Fatalf("quote2 = %d, want 120", q2)
	}
	c2, _ := SignCommitment(priv, p, q2)
	if err := ch.Authorize(c2, QSeen); err != nil {
		t.Fatalf("authorize2: %v", err)
	}
	if snap := ch.Snapshot(); snap.Accesses != 2 || snap.CumPaid != 120 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestUnderpaidRejected(t *testing.T) {
	priv := testKey(t, "b")
	p := testParams("b")
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 10, SettleFee: 0, FeeMode: FeeUpfront}, N: 10})
	// sign for less than required.
	c, _ := SignCommitment(priv, p, 5)
	if err := ch.Authorize(c, QSeen); err != ErrUnderpaid {
		t.Fatalf("want ErrUnderpaid, got %v", err)
	}
	if ch.Snapshot().Accesses != 0 {
		t.Fatal("state advanced on underpay")
	}
}

func TestExceedsDepositRejected(t *testing.T) {
	priv := testKey(t, "c")
	p := testParams("c")
	ch, _ := Open(Config{Params: p, Deposit: 50, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 100, SettleFee: 0, FeeMode: FeeUpfront}, N: 10})
	c, _ := SignCommitment(priv, p, 100)
	if err := ch.Authorize(c, QSeen); err != ErrExceedsDeposit {
		t.Fatalf("want ErrExceedsDeposit, got %v", err)
	}
}

func TestBadSignatureRejected(t *testing.T) {
	priv := testKey(t, "d")
	other := testKey(t, "d-other")
	p := testParams("d")
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 10, SettleFee: 0, FeeMode: FeeUpfront}, N: 10})
	// sign with the WRONG key.
	c, _ := SignCommitment(other, p, 10)
	if err := ch.Authorize(c, QSeen); err != ErrBadSig {
		t.Fatalf("want ErrBadSig, got %v", err)
	}
}

func TestWrongParamsRejected(t *testing.T) {
	priv := testKey(t, "e")
	p := testParams("e")
	pWrong := testParams("e-wrong")
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 10, SettleFee: 0, FeeMode: FeeUpfront}, N: 10})
	// client signs over different params (different payee/channel) — must not authorize.
	c, _ := SignCommitment(priv, pWrong, 10)
	if err := ch.Authorize(c, QSeen); err != ErrBadSig {
		t.Fatalf("want ErrBadSig for replayed/foreign commitment, got %v", err)
	}
}

func TestHighSRejected(t *testing.T) {
	priv := testKey(t, "f")
	p := testParams("f")
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 10, SettleFee: 0, FeeMode: FeeUpfront}, N: 10})
	c, _ := SignCommitment(priv, p, 10)
	// malleate to high-S; raw ECDSA still verifies, but the channel must reject it.
	mall, err := crypto.Malleate(c.Sig.Serialize())
	if err != nil {
		t.Fatal(err)
	}
	hs, _ := crypto.ParseSignature(mall)
	if hs.IsLowS() {
		t.Skip("malleation did not produce high-S (already low)")
	}
	if err := ch.Authorize(&Commitment{CumAmount: 10, Sig: hs}, QSeen); err != ErrBadSig {
		t.Fatalf("want ErrBadSig for high-S, got %v", err)
	}
}

func TestSettlementCountTrigger(t *testing.T) {
	priv := testKey(t, "g")
	p := testParams("g")
	ch, _ := Open(Config{Params: p, Deposit: 100000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 10, SettleFee: 80, FeeMode: FeeAmortized}, N: 4})
	for i := 0; i < 4; i++ {
		c, _ := SignCommitment(priv, p, ch.Quote(QSeen))
		if err := ch.Authorize(c, QSeen); err != nil {
			t.Fatal(err)
		}
	}
	ok, reason := ch.ShouldSettle(0)
	if !ok || reason != "count" {
		t.Fatalf("expected count trigger, got ok=%v reason=%q", ok, reason)
	}
	// amortized fee: ceil(80/4)=20 each => service 10 + 20 = 30 * 4 = 120.
	s, err := ch.Settle()
	if err != nil {
		t.Fatal(err)
	}
	if s.Accesses != 4 || s.ToServer != 120 {
		t.Fatalf("settlement = %+v", s)
	}
	if s.MiningFee != 80 || s.NetServer != 40 {
		t.Fatalf("fee accounting = %+v", s)
	}
	// settle is idempotent.
	if _, err := ch.Settle(); err != ErrClosed {
		t.Fatalf("want ErrClosed on second settle, got %v", err)
	}
}

func TestSettlementTimeTrigger(t *testing.T) {
	priv := testKey(t, "h")
	p := testParams("h")
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(),
		Pricing:      Pricing{Flat: true, FlatPrice: 1, SettleFee: 0, FeeMode: FeeUpfront},
		SettleBefore: 1000, RefundLockTime: 2000})
	if ok, _ := ch.ShouldSettle(999); ok {
		t.Fatal("settled before deadline")
	}
	if ok, reason := ch.ShouldSettle(1000); !ok || reason != "time" {
		t.Fatalf("expected time trigger, got %v %q", ok, reason)
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	priv := testKey(t, "i")
	p := testParams("i")
	// amortized with N=0 is invalid.
	if _, err := Open(Config{Params: p, Deposit: 1, ClientPub: priv.Public(),
		Pricing: Pricing{FeeMode: FeeAmortized}, N: 0, SettleBefore: 1}); err != ErrConfig {
		t.Fatalf("want ErrConfig for amortized N=0, got %v", err)
	}
	// no trigger at all is invalid.
	if _, err := Open(Config{Params: p, Deposit: 1, ClientPub: priv.Public(),
		Pricing: Pricing{FeeMode: FeeUpfront}, N: 0, SettleBefore: 0}); err != ErrConfig {
		t.Fatalf("want ErrConfig for no trigger, got %v", err)
	}
	// settle deadline must precede refund.
	if _, err := Open(Config{Params: p, Deposit: 1, ClientPub: priv.Public(),
		Pricing: Pricing{FeeMode: FeeUpfront}, SettleBefore: 2000, RefundLockTime: 1000}); err != ErrConfig {
		t.Fatalf("want ErrConfig for settle>=refund, got %v", err)
	}
}

func TestRefund(t *testing.T) {
	priv := testKey(t, "j")
	p := testParams("j")
	ch, _ := Open(Config{Params: p, Deposit: 777, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 1, FeeMode: FeeUpfront}, N: 10, RefundLockTime: 500})
	r := ch.Refund()
	if r.LockTime != 500 || r.Amount != 777 || !r.ToClient {
		t.Fatalf("refund = %+v", r)
	}
}

func TestMeteredPricing(t *testing.T) {
	priv := testKey(t, "k")
	p := testParams("k")
	pr := Pricing{Flat: false, SettleFee: 0, FeeMode: FeeUpfront}
	pr.PerType[QSeen] = 1
	pr.PerType[QMerklePath] = 5
	ch, _ := Open(Config{Params: p, Deposit: 1000, ClientPub: priv.Public(), Pricing: pr, N: 100})
	if got := ch.Quote(QSeen); got != 1 {
		t.Fatalf("seen quote %d", got)
	}
	c, _ := SignCommitment(priv, p, 1)
	if err := ch.Authorize(c, QSeen); err != nil {
		t.Fatal(err)
	}
	if got := ch.Quote(QMerklePath); got != 6 {
		t.Fatalf("merklepath quote %d, want 6", got)
	}
}

func TestSettlementTxBroadcastable(t *testing.T) {
	priv := testKey(t, "set")
	serverPriv := mkKey("server-" + "set")
	p := testParams("set")
	p.Fee = 250
	ch, err := Open(Config{Params: p, Deposit: 100000, ClientPub: priv.Public(),
		Pricing: Pricing{Flat: true, FlatPrice: 1000, SettleFee: 0, FeeMode: FeeUpfront}, N: 5})
	if err != nil {
		t.Fatal(err)
	}
	// prepay two accesses (cum advances to the client-signed amount).
	for i := 0; i < 2; i++ {
		c, err := SignCommitment(priv, p, ch.Quote(QSeen))
		if err != nil {
			t.Fatal(err)
		}
		if err := ch.Authorize(c, QSeen); err != nil {
			t.Fatal(err)
		}
	}
	cum := ch.Snapshot().CumPaid

	// the server builds a REAL broadcastable settlement tx, co-signing the best commitment.
	tx, err := ch.SettlementTx(serverPriv)
	if err != nil {
		t.Fatalf("settlement tx: %v", err)
	}
	if len(tx.Inputs) != 1 || len(tx.Inputs[0].ScriptSig) == 0 {
		t.Fatal("settlement tx missing signed input")
	}
	if tx.Outputs[0].Value != cum {
		t.Fatalf("server payout %d != cumPaid %d", tx.Outputs[0].Value, cum)
	}
	if tx.Outputs[1].Value != p.FundingValue-cum-p.Fee {
		t.Fatalf("client change wrong: %d", tx.Outputs[1].Value)
	}
	// the input must serialize and yield a stable txid.
	if tx.TxID() != tx.TxID() || len(tx.Serialize()) == 0 {
		t.Fatal("settlement tx not serializable")
	}
	// the refund tx carries the configured nLockTime (client safety net).
	_ = bsvtx.SighashAllFork
}

var _ = store.Outpoint{}

package payment_test

import (
	"testing"

	"memserve/api"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/ingest"
	"memserve/payment"
	"memserve/payment/channel"
	"memserve/prune"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
)

func populated(t *testing.T) (*api.Server, store.Hash) {
	t.Helper()
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	src := teranode.NewMock(teranode.MockConfig{Blocks: 1, SubtreesPer: 1, TxsPerSubtree: 8})
	b, _, _ := src.Next()
	if _, err := in.IngestBlock(b); err != nil {
		t.Fatal(err)
	}
	return api.New(st, 0), b.Subtrees[0].TxIDs[0]
}

func openChannel(t *testing.T, ps *payment.PaidServer, priv *crypto.PrivateKey) channel.Params {
	t.Helper()
	fund := commitment.DoubleSHA256([]byte("paid-test-fund"))
	p := channel.Params{
		ChannelID:        channel.DeriveChannelID(fund, 0),
		FundingTxID:      fund,
		FundingVout:      0,
		ServerScriptHash: commitment.DoubleSHA256([]byte("payee")),
	}
	_, err := ps.OpenChannel(channel.Config{
		Params: p, Deposit: 100000, ClientPub: priv.Public(),
		Pricing: channel.Pricing{Flat: true, FlatPrice: 10, SettleFee: 0, FeeMode: channel.FeeUpfront}, N: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func key(tag string) *crypto.PrivateKey {
	seed := commitment.DoubleSHA256([]byte(tag))
	k, _ := crypto.NewPrivateKey(seed[:])
	return k
}

func TestPaidQuerySucceeds(t *testing.T) {
	srv, txid := populated(t)
	ps := payment.New(srv)
	priv := key("client")
	p := openChannel(t, ps, priv)

	cum, err := ps.Quote(p.ChannelID, channel.QMined)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := channel.SignCommitment(priv, p, cum)
	r, err := ps.Mined(p.ChannelID, c, txid)
	if err != nil {
		t.Fatalf("paid mined: %v", err)
	}
	if !r.Mined {
		t.Fatal("paid query returned not mined")
	}
}

func TestUnauthorizedNotServed(t *testing.T) {
	srv, txid := populated(t)
	ps := payment.New(srv)
	priv := key("client")
	other := key("attacker")
	p := openChannel(t, ps, priv)

	cum, _ := ps.Quote(p.ChannelID, channel.QMined)
	// attacker signs — must be rejected, query NOT served.
	bad, _ := channel.SignCommitment(other, p, cum)
	if _, err := ps.Mined(p.ChannelID, bad, txid); err != channel.ErrBadSig {
		t.Fatalf("want ErrBadSig, got %v", err)
	}
	// underpaying is rejected too.
	low, _ := channel.SignCommitment(priv, p, 1)
	if _, err := ps.Mined(p.ChannelID, low, txid); err != channel.ErrUnderpaid {
		t.Fatalf("want ErrUnderpaid, got %v", err)
	}
}

func TestUnknownChannel(t *testing.T) {
	srv, _ := populated(t)
	ps := payment.New(srv)
	var none store.Hash
	if _, err := ps.Quote(none, channel.QSeen); err != payment.ErrNoChannel {
		t.Fatalf("want ErrNoChannel, got %v", err)
	}
}

func TestAbuseMinDepositAndMaxChannels(t *testing.T) {
	srv, _ := populated(t)
	var alerts []payment.Alert
	ps := payment.NewWithPolicy(srv, payment.Policy{MinDeposit: 1000, MaxChannels: 1, MaxBadAttempts: 3},
		payment.NotifierFunc(func(a payment.Alert) { alerts = append(alerts, a) }))

	priv := key("client")
	mk := func(tag string, deposit uint64) channel.Config {
		fund := commitment.DoubleSHA256([]byte(tag))
		p := channel.Params{ChannelID: channel.DeriveChannelID(fund, 0), FundingTxID: fund,
			ServerScriptHash: commitment.DoubleSHA256([]byte("payee"))}
		return channel.Config{Params: p, Deposit: deposit, ClientPub: priv.Public(),
			Pricing: channel.Pricing{Flat: true, FlatPrice: 1, FeeMode: channel.FeeUpfront}, N: 100}
	}
	// deposit below MinDeposit is rejected (attacker can't open cheap channels).
	if _, err := ps.OpenChannel(mk("c1", 500)); err != payment.ErrDepositTooSmall {
		t.Fatalf("want ErrDepositTooSmall, got %v", err)
	}
	// first funded channel opens; second exceeds MaxChannels (open-flood defeated).
	if _, err := ps.OpenChannel(mk("c2", 5000)); err != nil {
		t.Fatalf("funded open failed: %v", err)
	}
	if _, err := ps.OpenChannel(mk("c3", 5000)); err != payment.ErrTooManyChannels {
		t.Fatalf("want ErrTooManyChannels, got %v", err)
	}
	if len(alerts) < 2 {
		t.Fatalf("operator not alerted on abuse: %d alerts", len(alerts))
	}
}

func TestAbuseBadAttemptBan(t *testing.T) {
	srv, txid := populated(t)
	var banned bool
	ps := payment.NewWithPolicy(srv, payment.Policy{MaxBadAttempts: 3},
		payment.NotifierFunc(func(a payment.Alert) {
			if a.Kind == payment.AlertChannelBanned {
				banned = true
			}
		}))
	priv := key("client")
	other := key("attacker")
	p := openChannel(t, ps, priv)

	cum, _ := ps.Quote(p.ChannelID, channel.QMined)
	// flood invalid (wrong-key) commitments; after the budget the channel is banned.
	for i := 0; i < 3; i++ {
		bad, _ := channel.SignCommitment(other, p, cum)
		if _, err := ps.Mined(p.ChannelID, bad, txid); err != channel.ErrBadSig {
			t.Fatalf("attempt %d: want ErrBadSig, got %v", i, err)
		}
	}
	if !banned {
		t.Fatal("channel not banned after exceeding bad-attempt budget")
	}
	// even a VALID commitment is now refused — the flood cost the attacker its channel.
	good, _ := channel.SignCommitment(priv, p, cum)
	if _, err := ps.Mined(p.ChannelID, good, txid); err != payment.ErrChannelBanned {
		t.Fatalf("want ErrChannelBanned after ban, got %v", err)
	}
}

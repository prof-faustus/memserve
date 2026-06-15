package bsvtx

import (
	"bytes"
	"testing"

	"memserve/commitment"
	"memserve/crypto"
)

func key(tag string) *crypto.PrivateKey {
	seed := commitment.DoubleSHA256([]byte(tag))
	k, _ := crypto.NewPrivateKey(seed[:])
	return k
}

func TestSerializeTxIDDeterministic(t *testing.T) {
	mk := func() *Tx {
		var h [32]byte
		h[0] = 0x11
		return &Tx{Version: 2,
			Inputs:  []TxIn{{PrevOut: OutPoint{Hash: h, Index: 1}, Sequence: FinalSequence}},
			Outputs: []TxOut{{Value: 5000, ScriptPubKey: P2PK(key("a").Public().SerializeCompressed())}},
		}
	}
	a, b := mk(), mk()
	if a.TxID() != b.TxID() {
		t.Fatal("txid not deterministic")
	}
	// display id is the byte-reverse of the internal id.
	id, disp := a.TxID(), a.DisplayID()
	for i := 0; i < 32; i++ {
		if id[i] != disp[31-i] {
			t.Fatal("DisplayID is not the reverse of TxID")
		}
	}
}

func TestDEREncodeDecode(t *testing.T) {
	priv := key("der")
	h := commitment.DoubleSHA256([]byte("msg"))
	sig, _ := priv.Sign(h[:])
	der := derEncode(sig.R, sig.S)
	r, s, err := derDecode(der)
	if err != nil || r.Cmp(sig.R) != 0 || s.Cmp(sig.S) != 0 {
		t.Fatalf("DER round trip failed: %v", err)
	}
}

func TestSighashForkIDDeterministic(t *testing.T) {
	c := testChannel()
	h1, err := c.CommitmentSighash(3000)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := c.CommitmentSighash(3000)
	if !bytes.Equal(h1, h2) {
		t.Fatal("FORKID sighash not deterministic")
	}
	h3, _ := c.CommitmentSighash(4000)
	if bytes.Equal(h1, h3) {
		t.Fatal("sighash did not change with the amount")
	}
}

func testChannel() ChannelTx {
	var fund [32]byte
	fund[0] = 0xAB
	return ChannelTx{
		FundingOut:   OutPoint{Hash: fund, Index: 0},
		FundingValue: 100000,
		ClientPub:    key("client").Public().SerializeCompressed(),
		ServerPub:    key("server").Public().SerializeCompressed(),
		Fee:          500,
	}
}

func TestCommitmentSignVerifyTwoOfTwo(t *testing.T) {
	clientPriv, serverPriv := key("client"), key("server")
	c := testChannel()
	tx, err := c.CommitmentTx(3000)
	if err != nil {
		t.Fatal(err)
	}
	// both parties sign the same FORKID sighash over the 2-of-2 redeem.
	clientSig, err := SignInput(clientPriv, tx, 0, c.Redeem(), c.FundingValue, SighashAllFork)
	if err != nil {
		t.Fatal(err)
	}
	serverSig, _ := SignInput(serverPriv, tx, 0, c.Redeem(), c.FundingValue, SighashAllFork)

	if !VerifyInput(clientPriv.Public(), tx, 0, c.Redeem(), c.FundingValue, clientSig) {
		t.Fatal("client signature does not verify")
	}
	if !VerifyInput(serverPriv.Public(), tx, 0, c.Redeem(), c.FundingValue, serverSig) {
		t.Fatal("server signature does not verify")
	}
	// wrong key must fail.
	if VerifyInput(key("attacker").Public(), tx, 0, c.Redeem(), c.FundingValue, clientSig) {
		t.Fatal("verify accepted a foreign key")
	}
	// finalize -> broadcastable scriptSig present, tx serializes & has a stable id.
	Finalize2of2(tx, clientSig, serverSig)
	if len(tx.Inputs[0].ScriptSig) == 0 {
		t.Fatal("scriptSig not set after finalize")
	}
	if len(tx.Serialize()) == 0 {
		t.Fatal("finalized tx did not serialize")
	}
}

func TestHighSRejectedInVerifyInput(t *testing.T) {
	priv := key("client")
	c := testChannel()
	tx, _ := c.CommitmentTx(3000)
	sig, _ := SignInput(priv, tx, 0, c.Redeem(), c.FundingValue, SighashAllFork)
	// flip to high-S: decode, malleate S -> n-S, re-encode, re-append hashtype.
	r, s, _ := derDecode(sig[:len(sig)-1])
	ht := sig[len(sig)-1]
	mall, _ := crypto.Malleate((&crypto.Signature{R: r, S: s}).Serialize())
	hs, _ := crypto.ParseSignature(mall)
	if hs.IsLowS() {
		t.Skip("already low-S")
	}
	bad := append(derEncode(hs.R, hs.S), ht)
	if VerifyInput(priv.Public(), tx, 0, c.Redeem(), c.FundingValue, bad) {
		t.Fatal("high-S signature accepted (malleable)")
	}
}

func TestRefundHasLockTime(t *testing.T) {
	c := testChannel()
	rt := c.RefundTx(800000)
	if rt.LockTime != 800000 {
		t.Fatalf("refund locktime = %d", rt.LockTime)
	}
	if rt.Inputs[0].Sequence != EnableLockTimeSequence {
		t.Fatal("refund input sequence does not enable nLockTime")
	}
	// refund pays the client the funded value minus fee.
	if rt.Outputs[0].Value != c.FundingValue-c.Fee {
		t.Fatalf("refund value = %d", rt.Outputs[0].Value)
	}
}

func TestCommitmentAmountGuard(t *testing.T) {
	c := testChannel()
	if _, err := c.CommitmentTx(c.FundingValue); err != ErrAmount {
		t.Fatalf("want ErrAmount when toServer+fee exceeds funding, got %v", err)
	}
}

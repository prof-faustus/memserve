package bsvtx

import (
	"bytes"
	"testing"
)

func TestBase58CheckKnownVector(t *testing.T) {
	// Mainnet P2PKH address of the all-zero hash160 is a widely-cited constant.
	got := Base58Check(append([]byte{AddrMainP2PKH}, make([]byte, 20)...))
	if got != "1111111111111111111114oLvT2" {
		t.Fatalf("Base58Check(zero hash160) = %s", got)
	}
}

func TestBase58RoundTrip(t *testing.T) {
	for _, in := range [][]byte{{0x00, 0x01, 0x02}, {0x6f, 0xde, 0xad, 0xbe, 0xef}, {0, 0, 5, 9}} {
		enc := Base58Encode(in)
		dec, err := Base58Decode(enc)
		if err != nil || !bytes.Equal(dec, in) {
			t.Fatalf("round trip %x -> %s -> %x (%v)", in, enc, dec, err)
		}
	}
}

func TestBase58CheckDetectsCorruption(t *testing.T) {
	addr := Base58Check(append([]byte{AddrTestP2PKH}, make([]byte, 20)...))
	if _, err := Base58CheckDecode(addr); err != nil {
		t.Fatalf("valid address failed decode: %v", err)
	}
	// flip a character -> checksum must fail.
	b := []byte(addr)
	if b[5] == 'A' {
		b[5] = 'B'
	} else {
		b[5] = 'A'
	}
	if _, err := Base58CheckDecode(string(b)); err == nil {
		t.Fatal("corrupted address passed checksum")
	}
}

func TestAddressAndP2PKH(t *testing.T) {
	pub := key("addr").Public().SerializeCompressed()
	addr := AddressFromPubKey(pub, AddrTestP2PKH)
	if len(addr) == 0 || addr[0] != 'm' && addr[0] != 'n' {
		t.Fatalf("testnet P2PKH address has unexpected form: %s", addr)
	}
	// decoding the address yields version || hash160(pub).
	payload, err := Base58CheckDecode(addr)
	if err != nil || len(payload) != 21 || payload[0] != AddrTestP2PKH {
		t.Fatalf("address decode: %x err=%v", payload, err)
	}
	h := Hash160(pub)
	if !bytes.Equal(payload[1:], h[:]) {
		t.Fatal("address hash160 mismatch")
	}
	// the P2PKH script embeds the same hash160.
	script := P2PKHFromPub(pub)
	if len(script) != 25 || script[0] != 0x76 || script[1] != 0xa9 || script[2] != 0x14 || script[23] != 0x88 || script[24] != 0xac {
		t.Fatalf("P2PKH script malformed: %x", script)
	}
	if !bytes.Equal(script[3:23], h[:]) {
		t.Fatal("P2PKH script hash160 mismatch")
	}
}

func TestP2PKHSpendSignVerify(t *testing.T) {
	priv := key("spender")
	pub := priv.Public().SerializeCompressed()
	var prev [32]byte
	prev[0] = 0x42
	tx := &Tx{Version: 2,
		Inputs:  []TxIn{{PrevOut: OutPoint{Hash: prev, Index: 0}, Sequence: FinalSequence}},
		Outputs: []TxOut{{Value: 9000, ScriptPubKey: P2PKHFromPub(pub)}},
	}
	scriptCode := P2PKHFromPub(pub) // the output being spent is P2PKH to this key
	sig, err := SignInput(priv, tx, 0, scriptCode, 10000, SighashAllFork)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyInput(priv.Public(), tx, 0, scriptCode, 10000, sig) {
		t.Fatal("P2PKH FORKID signature did not verify")
	}
	tx.Inputs[0].ScriptSig = ScriptSigP2PKH(sig, pub)
	if len(tx.Inputs[0].ScriptSig) == 0 || len(tx.Serialize()) == 0 {
		t.Fatal("P2PKH scriptSig/serialize failed")
	}
}

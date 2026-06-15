package server_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"memserve/attest"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/payment"
	"memserve/payment/channel"
	"memserve/prune"
	"memserve/server"
	"memserve/teranode"
)

func newTestServer(t *testing.T, paymentRequired bool) (*server.Server, *httptest.Server, teranode.Block) {
	t.Helper()
	src := teranode.NewMock(teranode.MockConfig{Blocks: 3, SubtreesPer: 2, TxsPerSubtree: 16, SpendFraction: 2})
	// keep a copy of block 0 for known txids.
	probe := teranode.NewMock(teranode.MockConfig{Blocks: 3, SubtreesPer: 2, TxsPerSubtree: 16, SpendFraction: 2})
	b0, _, _ := probe.Next()

	seed := commitment.DoubleSHA256([]byte("operator"))
	pol, _ := prune.PolicyWithD(6, 6)
	srv, err := server.New(server.Config{
		ShardK: 0, Prune: pol, Abuse: payment.DefaultPolicy(),
		PaymentRequired: paymentRequired, OperatorSeed: seed[:], AdminToken: "secret",
	}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.IngestOnce(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, b0
}

func getJSON(t *testing.T, url string, dst any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if dst != nil && resp.StatusCode == 200 {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode
}

func TestHealthReadyMetrics(t *testing.T) {
	_, ts, _ := newTestServer(t, false)
	if code := getJSON(t, ts.URL+"/healthz", nil); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	if code := getJSON(t, ts.URL+"/readyz", nil); code != 200 {
		t.Fatalf("readyz = %d", code)
	}
	resp, _ := http.Get(ts.URL + "/metrics")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "memserve_tip_height") || !strings.Contains(string(body), "memserve_revenue_satoshis") {
		t.Fatalf("metrics missing fields:\n%s", body)
	}
}

func TestSeenAndSignedAttestation(t *testing.T) {
	srv, ts, b0 := newTestServer(t, false)
	txid := b0.Subtrees[0].TxIDs[0]
	var resp server.SeenResponse
	if code := getJSON(t, ts.URL+"/v1/seen?txid="+hex.EncodeToString(txid[:]), &resp); code != 200 {
		t.Fatalf("seen = %d", code)
	}
	if !resp.Seen || resp.Attestation == nil {
		t.Fatalf("seen=%v att=%v", resp.Seen, resp.Attestation)
	}
	// the signed attestation must verify under the server's operator key.
	opPub, _ := hex.DecodeString(srv.OperatorPubHex())
	attPub, _ := hex.DecodeString(resp.Attestation.Operator)
	if !bytes.Equal(opPub, attPub) {
		t.Fatal("attestation operator != server operator")
	}
	att := decodeAtt(t, *resp.Attestation)
	if !att.Verify() {
		t.Fatal("signed attestation does not verify")
	}
}

func TestMerklePathOverWireVerifies(t *testing.T) {
	_, ts, b0 := newTestServer(t, false)
	txid := b0.Subtrees[0].TxIDs[0]
	var resp server.MerklePathResponse
	if code := getJSON(t, ts.URL+"/v1/merklepath?txid="+hex.EncodeToString(txid[:]), &resp); code != 200 {
		t.Fatalf("merklepath = %d", code)
	}
	p, ok := server.DecodeProof(resp.Proof)
	if !ok || !p.Verify() || p.Leaf != txid {
		t.Fatal("proof from the wire did not verify")
	}
}

func TestPaymentRequiredGate(t *testing.T) {
	_, ts, b0 := newTestServer(t, true)
	txid := b0.Subtrees[0].TxIDs[0]
	if code := getJSON(t, ts.URL+"/v1/seen?txid="+hex.EncodeToString(txid[:]), nil); code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 when payment required, got %d", code)
	}
}

func TestPaidQueryFlow(t *testing.T) {
	srv, ts, b0 := newTestServer(t, true)
	txid := b0.Subtrees[0].TxIDs[0]

	// open a channel in-process (HTTP open also exists; this keeps the test focused).
	clientSeed := commitment.DoubleSHA256([]byte("client"))
	priv, _ := crypto.NewPrivateKey(clientSeed[:])
	fund := commitment.DoubleSHA256([]byte("funding"))
	params := channel.Params{ChannelID: channel.DeriveChannelID(fund, 0), FundingTxID: fund,
		ServerScriptHash: commitment.DoubleSHA256([]byte("payee"))}
	_, err := srv.Paid().OpenChannel(channel.Config{
		Params: params, Deposit: 100000, ClientPub: priv.Public(),
		Pricing: channel.Pricing{Flat: true, FlatPrice: 5, FeeMode: channel.FeeUpfront}, N: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// quote then prepay then query (mined).
	cum, err := srv.Paid().Quote(params.ChannelID, channel.QMined)
	if err != nil {
		t.Fatal(err)
	}
	commit, _ := channel.SignCommitment(priv, params, cum)
	body, _ := json.Marshal(map[string]any{
		"channel": hex.EncodeToString(params.ChannelID[:]), "type": 1,
		"txid": hex.EncodeToString(txid[:]), "cumAmount": commit.CumAmount,
		"sig": hex.EncodeToString(commit.Sig.Serialize()),
	})
	resp, err := http.Post(ts.URL+"/v1/paid/query", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("paid query = %d: %s", resp.StatusCode, b)
	}
	var mr server.MinedResponse
	json.NewDecoder(resp.Body).Decode(&mr)
	if !mr.Mined {
		t.Fatal("paid mined query returned not mined")
	}
	// revenue recorded (miner value-add).
	if srv.Paid().Revenue() == 0 {
		t.Fatal("no revenue recorded after paid query")
	}
}

func TestAdminAuth(t *testing.T) {
	_, ts, _ := newTestServer(t, false)
	if code := getJSON(t, ts.URL+"/admin/stats", nil); code != http.StatusUnauthorized {
		t.Fatalf("admin without token = %d, want 401", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/admin/stats", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("admin with token = %d", resp.StatusCode)
	}
}

// decodeAtt reconstructs an attest.Attestation from its wire form for verification.
func decodeAtt(t *testing.T, j server.AttestationJSON) attest.Attestation {
	t.Helper()
	txid, _ := hex.DecodeString(j.TxID)
	bh, _ := hex.DecodeString(j.BlockHash)
	opb, _ := hex.DecodeString(j.Operator)
	sb, _ := hex.DecodeString(j.Sig)
	pub, err := crypto.ParseCompressed(opb)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := crypto.ParseSignature(sb)
	if err != nil {
		t.Fatal(err)
	}
	var st attest.Statement
	st.Kind = attest.Kind(j.Kind)
	copy(st.TxID[:], txid)
	copy(st.BlockHash[:], bh)
	st.Vout = j.Vout
	st.Flag = j.Flag
	st.Height = j.Height
	st.Tip = j.Tip
	return attest.Attestation{Statement: st, Operator: pub, Sig: sig}
}

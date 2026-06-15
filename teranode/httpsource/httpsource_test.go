package httpsource_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"memserve/api"
	"memserve/ingest"
	"memserve/proof"
	"memserve/prune"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
	"memserve/teranode/httpsource"
)

// JSON contract mirrors httpsource's adapter shape.
type jIn struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}
type jOut struct {
	Value      uint64 `json:"value"`
	ScriptHash string `json:"scriptHash"`
}
type jTx struct {
	TxID    string `json:"txid"`
	Inputs  []jIn  `json:"inputs"`
	Outputs []jOut `json:"outputs"`
}
type jSub struct {
	Root string `json:"root"`
	Txs  []jTx  `json:"txs"`
}
type jBlk struct {
	Hash     string `json:"hash"`
	Height   uint32 `json:"height"`
	Time     uint32 `json:"time"`
	Header   string `json:"header"`
	Subtrees []jSub `json:"subtrees"`
}

func hx(h store.Hash) string { return hex.EncodeToString(h[:]) }

func toJSON(b teranode.Block) jBlk {
	jb := jBlk{Hash: hx(b.Hash), Height: b.Height, Time: b.Time, Header: hex.EncodeToString(b.Header[:])}
	for _, sub := range b.Subtrees {
		js := jSub{Root: hx(sub.Root)}
		for _, tx := range sub.Txs {
			jt := jTx{TxID: hx(tx.TxID)}
			for _, in := range tx.Inputs {
				jt.Inputs = append(jt.Inputs, jIn{TxID: hx(in.TxID), Vout: in.Vout})
			}
			for _, o := range tx.Outputs {
				jt.Outputs = append(jt.Outputs, jOut{Value: o.Value, ScriptHash: hx(o.ScriptHash)})
			}
			js.Txs = append(js.Txs, jt)
		}
		jb.Subtrees = append(jb.Subtrees, js)
	}
	return jb
}

// fakeTeranode serves the adapter contract from a set of mock blocks.
func fakeTeranode(t *testing.T, blocks []teranode.Block) *httptest.Server {
	byHeight := map[uint32]jBlk{}
	var tip uint32
	for _, b := range blocks {
		byHeight[b.Height] = toJSON(b)
		if b.Height > tip {
			tip = b.Height
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/bestheight", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]uint32{"height": tip})
	})
	mux.HandleFunc("/api/v1/block/", func(w http.ResponseWriter, r *http.Request) {
		hs := strings.TrimPrefix(r.URL.Path, "/api/v1/block/")
		h, _ := strconv.Atoi(hs)
		jb, ok := byHeight[uint32(h)]
		if !ok {
			http.Error(w, "no such block", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(jb)
	})
	return httptest.NewServer(mux)
}

func TestHTTPSourceIngestEndToEnd(t *testing.T) {
	// build deterministic mock blocks, serve them as a fake Teranode.
	src := teranode.NewMock(teranode.MockConfig{Blocks: 4, SubtreesPer: 2, TxsPerSubtree: 16, SpendFraction: 2})
	var blocks []teranode.Block
	for {
		b, ok, _ := src.Next()
		if !ok {
			break
		}
		blocks = append(blocks, b)
	}
	ts := fakeTeranode(t, blocks)
	defer ts.Close()

	// pull from the HTTP source through the real ingest path (with validation ON).
	hs := httpsource.New(httpsource.Config{BaseURL: ts.URL, StartHeight: 0})
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	total, err := in.Run(hs)
	if err != nil {
		t.Fatalf("ingest from HTTP source failed: %v", err)
	}
	if total.Blocks != 4 {
		t.Fatalf("ingested %d blocks, want 4", total.Blocks)
	}

	// a tx pulled over HTTP must be queryable and its proof must verify.
	srv := api.New(st, 0)
	txid := blocks[0].Subtrees[0].TxIDs[0]
	if r, _ := srv.Seen(txid); !r.Seen {
		t.Fatal("HTTP-ingested tx not seen")
	}
	p, err := proof.Build(st, txid)
	if err != nil || !p.Verify() {
		t.Fatalf("HTTP-ingested proof does not verify: %v", err)
	}
	if hs.TipHeight() != 3 {
		t.Fatalf("tip = %d, want 3", hs.TipHeight())
	}
}

func TestHTTPSourceRejectsCorruptHeader(t *testing.T) {
	// a block whose header doesn't commit to its txs must be rejected by ingest validation.
	src := teranode.NewMock(teranode.MockConfig{Blocks: 1, SubtreesPer: 1, TxsPerSubtree: 4})
	b, _, _ := src.Next()
	b.Header[40] ^= 0xFF // corrupt the committed merkle root in the header
	ts := fakeTeranode(t, []teranode.Block{b})
	defer ts.Close()
	hs := httpsource.New(httpsource.Config{BaseURL: ts.URL, StartHeight: 0, EndHeight: 0})
	st := mem.New()
	in := ingest.New(st, prune.New(st, prune.Policy{}), ingest.Config{})
	_, err := in.Run(hs)
	if err != ingest.ErrInvalidBlock {
		t.Fatalf("corrupt block from HTTP not rejected: %v", err)
	}
	_ = fmt.Sprint
}

// Package httpsource is the real Teranode ingest adapter: an HTTP client that pulls
// sealed blocks (with their subtrees and txs) from a Teranode asset/data server and
// presents them as a teranode.Source, so the production ingest path is identical to the
// mock (DESIGN.md §17). The JSON shape below is MemServe's adapter contract; pointing it
// at a specific Teranode build is a thin endpoint/field-mapping step.
//
// It is tested against an httptest server that serves this contract (see the test), so
// the fetch/parse/stream logic is fully exercised offline; the only step that needs a
// live Teranode is the final endpoint mapping. Ingest still re-validates every block's
// Merkle consistency (anti-poisoning), so a faulty/hostile endpoint cannot corrupt state.
// BSV/Teranode only.
package httpsource

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"memserve/store"
	"memserve/teranode"
)

// Config parameters the adapter.
type Config struct {
	BaseURL     string        // Teranode asset/data server base, e.g. http://teranode:8090
	StartHeight uint32        // first block height to ingest
	EndHeight   uint32        // last height (0 = follow the tip indefinitely)
	HTTPClient  *http.Client  // optional; a sane default is used if nil
	PollEvery   time.Duration // how long to wait when caught up before re-checking tip

	// Endpoint mapping (defaults model a Teranode asset server). BlockPathFmt must
	// contain one %d for the height.
	BestHeightPath string // default "/api/v1/bestheight"
	BlockPathFmt   string // default "/api/v1/block/%d"

	// Resilience / auth.
	AuthBearer   string        // optional bearer token
	MaxRetries   int           // transient-error retries per request (default 4)
	RetryBackoff time.Duration // base backoff, exponential (default 200ms)
	MaxBodyBytes int64         // response body cap (default 64 MiB)
}

// Source is a teranode.Source backed by a Teranode HTTP endpoint.
type Source struct {
	cfg    Config
	hc     *http.Client
	height uint32
	tip    uint32
}

// New builds an HTTP Teranode source.
func New(cfg Config) *Source {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.BestHeightPath == "" {
		cfg.BestHeightPath = "/api/v1/bestheight"
	}
	if cfg.BlockPathFmt == "" {
		cfg.BlockPathFmt = "/api/v1/block/%d"
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 4
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 200 * time.Millisecond
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 64 << 20
	}
	return &Source{cfg: cfg, hc: hc, height: cfg.StartHeight}
}

// --- adapter JSON contract --------------------------------------------------

type jsonInput struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}
type jsonOutput struct {
	Value      uint64 `json:"value"`
	ScriptHash string `json:"scriptHash"`
}
type jsonTx struct {
	TxID    string       `json:"txid"`
	Inputs  []jsonInput  `json:"inputs"`
	Outputs []jsonOutput `json:"outputs"`
}
type jsonSubtree struct {
	Root string   `json:"root"`
	Txs  []jsonTx `json:"txs"`
}
type jsonBlock struct {
	Hash     string        `json:"hash"`
	Height   uint32        `json:"height"`
	Time     uint32        `json:"time"`
	Header   string        `json:"header"`
	Subtrees []jsonSubtree `json:"subtrees"`
}
type jsonTip struct {
	Height uint32 `json:"height"`
}

// TipHeight returns the last-known chain tip.
func (s *Source) TipHeight() uint32 { return s.tip }

// Next fetches the next block in height order. Returns ok=false when caught up to the tip
// (or past EndHeight).
func (s *Source) Next() (teranode.Block, bool, error) {
	if s.cfg.EndHeight != 0 && s.height > s.cfg.EndHeight {
		return teranode.Block{}, false, nil
	}
	if s.height > s.tip {
		if err := s.refreshTip(); err != nil {
			return teranode.Block{}, false, err
		}
		if s.height > s.tip {
			return teranode.Block{}, false, nil // caught up
		}
	}
	blk, err := s.fetchBlock(s.height)
	if err != nil {
		return teranode.Block{}, false, err
	}
	s.height++
	return blk, true, nil
}

func (s *Source) refreshTip() error {
	var t jsonTip
	if err := s.getJSON(s.cfg.BaseURL+s.cfg.BestHeightPath, &t); err != nil {
		return err
	}
	s.tip = t.Height
	return nil
}

func (s *Source) fetchBlock(h uint32) (teranode.Block, error) {
	var jb jsonBlock
	if err := s.getJSON(s.cfg.BaseURL+fmt.Sprintf(s.cfg.BlockPathFmt, h), &jb); err != nil {
		return teranode.Block{}, err
	}
	return convert(jb)
}

// getJSON GETs url and decodes JSON, retrying transient failures (network errors, 429,
// 5xx) with exponential backoff. 4xx (except 429) is a permanent error. The body is
// capped at MaxBodyBytes.
func (s *Source) getJSON(url string, dst any) error {
	var lastErr error
	backoff := s.cfg.RetryBackoff
	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff)
			backoff *= 2
		}
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return err // malformed URL is permanent
		}
		if s.cfg.AuthBearer != "" {
			req.Header.Set("Authorization", "Bearer "+s.cfg.AuthBearer)
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			lastErr = err // network error: retry
			continue
		}
		if resp.StatusCode == http.StatusOK {
			err := json.NewDecoder(io.LimitReader(resp.Body, s.cfg.MaxBodyBytes)).Decode(dst)
			resp.Body.Close()
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		lastErr = fmt.Errorf("httpsource: GET %s -> %d: %s", url, resp.StatusCode, string(body))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			continue // transient: retry
		}
		return lastErr // permanent 4xx
	}
	return fmt.Errorf("httpsource: exhausted %d retries: %w", s.cfg.MaxRetries, lastErr)
}

// --- conversion -------------------------------------------------------------

func hexHash(s string) (store.Hash, error) {
	var h store.Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, err
	}
	if len(b) != 32 {
		return h, fmt.Errorf("httpsource: hash must be 32 bytes, got %d", len(b))
	}
	copy(h[:], b)
	return h, nil
}

func convert(jb jsonBlock) (teranode.Block, error) {
	hb, err := hex.DecodeString(jb.Header)
	if err != nil || len(hb) != 80 {
		return teranode.Block{}, fmt.Errorf("httpsource: header must be 80 bytes hex")
	}
	var blk teranode.Block
	blk.Height = jb.Height
	blk.Time = jb.Time
	copy(blk.Header[:], hb)
	if blk.Hash, err = hexHash(jb.Hash); err != nil {
		return blk, err
	}
	for _, js := range jb.Subtrees {
		var sub teranode.Subtree
		if sub.Root, err = hexHash(js.Root); err != nil {
			return blk, err
		}
		for _, jt := range js.Txs {
			txid, err := hexHash(jt.TxID)
			if err != nil {
				return blk, err
			}
			tx := teranode.Tx{TxID: txid}
			for _, in := range jt.Inputs {
				itx, err := hexHash(in.TxID)
				if err != nil {
					return blk, err
				}
				tx.Inputs = append(tx.Inputs, store.Outpoint{TxID: itx, Vout: in.Vout})
			}
			for _, out := range jt.Outputs {
				sh, err := hexHash(out.ScriptHash)
				if err != nil {
					return blk, err
				}
				tx.Outputs = append(tx.Outputs, teranode.TxOut{Value: out.Value, ScriptHash: sh})
			}
			sub.TxIDs = append(sub.TxIDs, txid)
			sub.Txs = append(sub.Txs, tx)
		}
		blk.Subtrees = append(blk.Subtrees, sub)
		blk.SubtreeRoots = append(blk.SubtreeRoots, sub.Root)
	}
	// MerkleRoot from the header (bytes 36..68); ingest re-validates consistency.
	copy(blk.MerkleRoot[:], blk.Header[36:68])
	return blk, nil
}

var _ teranode.Source = (*Source)(nil)

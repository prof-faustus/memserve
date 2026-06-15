package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"memserve/api"
	"memserve/attest"
	"memserve/commitment"
	"memserve/crypto"
	"memserve/ingest"
	"memserve/payment"
	"memserve/prune"
	"memserve/store"
	"memserve/store/mem"
	"memserve/teranode"
)

// Config configures the MemServe server.
type Config struct {
	ListenAddr      string         // e.g. ":8080"
	ShardK          uint           // hash-prefix width (0 = single shard)
	ShardID         uint32         // this server's shard id
	Prune           prune.Policy   // spend-depth retention (use prune.RecommendedPolicy())
	Abuse           payment.Policy // channel abuse policy
	PaymentRequired bool           // if true, the four queries require a paid channel
	OperatorSeed    []byte         // 32 bytes; if set, answers are signed (accountability)
	AdminToken      string         // bearer token for /admin/*
	RatePerSec      int            // per-client request rate (0 = unlimited)
	RequestTimeout  time.Duration  // per-request timeout (default 15s)
	PollEvery       time.Duration  // ingest poll interval when caught up (default 1s)
	Store           store.Store    // backend; if nil, an in-memory store is used
	MaxMemMB        int            // pause ingestion when the process heap exceeds this (0 = unlimited)
}

// Server is a running MemServe shard service.
type Server struct {
	cfg      Config
	store    store.Store
	ing      *ingest.Ingestor
	api      *api.Server
	paid     *payment.PaidServer
	identity *attest.Identity
	payKey   *crypto.PrivateKey // server's payout/settlement key (channel ServerPub)
	src      teranode.Source
	log      *slog.Logger

	tip     atomic.Uint32
	ready   atomic.Bool
	httpSrv *http.Server
	limiter *limiter
	met     metrics
}

type metrics struct {
	requests atomic.Uint64
	errors   atomic.Uint64
	served   atomic.Uint64
	rejected atomic.Uint64 // payment/abuse rejections
	blocks   atomic.Uint64
}

// New builds a server over an in-memory store fed by src. (For Aerospike, swap the store
// construction; the rest is identical.)
func New(cfg Config, src teranode.Source, log *slog.Logger) (*Server, error) {
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 15 * time.Second
	}
	if cfg.PollEvery == 0 {
		cfg.PollEvery = time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	var st store.Store = cfg.Store
	if st == nil {
		st = mem.New()
	}
	pr := prune.New(st, cfg.Prune)
	ing := ingest.New(st, pr, ingest.Config{K: cfg.ShardK, ID: cfg.ShardID})
	apiSrv := api.New(st, cfg.Prune.D())
	s := &Server{
		cfg:   cfg,
		store: st,
		ing:   ing,
		api:   apiSrv,
		paid: payment.NewWithPolicy(apiSrv, cfg.Abuse, payment.NotifierFunc(func(a payment.Alert) {
			log.Warn("abuse alert", "kind", a.Kind.String(), "channel", hexHash(a.ChannelID), "count", a.Count)
		})),
		src: src,
		log: log,
	}
	if len(cfg.OperatorSeed) > 0 {
		id, err := attest.NewIdentity(cfg.OperatorSeed)
		if err != nil {
			return nil, err
		}
		s.identity = id
	}
	// Derive the server's payout key (channel ServerPub). From the operator seed when
	// set (distinct domain), else ephemeral for the process lifetime.
	paySeed := commitment.DoubleSHA256(append([]byte("memserve-paykey-v1"), cfg.OperatorSeed...))
	pk, err := crypto.NewPrivateKey(paySeed[:])
	if err != nil {
		return nil, err
	}
	s.payKey = pk
	if cfg.RatePerSec > 0 {
		s.limiter = newLimiter(cfg.RatePerSec)
	}
	return s, nil
}

// OperatorPubHex returns the operator's compressed pubkey hex (for clients to verify
// attestations), or "" if attestations are disabled.
func (s *Server) OperatorPubHex() string {
	if s.identity == nil {
		return ""
	}
	return hex.EncodeToString(s.identity.Public().SerializeCompressed())
}

// Paid exposes the payment server (for opening channels in-process / tests).
func (s *Server) Paid() *payment.PaidServer { return s.paid }

// Start begins ingestion and serves HTTP until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	go s.ingestLoop(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/seen", s.wrap(s.handleSeen))
	mux.HandleFunc("/v1/mined", s.wrap(s.handleMined))
	mux.HandleFunc("/v1/merklepath", s.wrap(s.handleMerklePath))
	mux.HandleFunc("/v1/utxo", s.wrap(s.handleUTXO))
	mux.HandleFunc("/v1/channel/open", s.wrap(s.handleChannelOpen))
	mux.HandleFunc("/v1/quote", s.wrap(s.handleQuote))
	mux.HandleFunc("/v1/paid/query", s.wrap(s.handlePaidQuery))
	mux.HandleFunc("/admin/stats", s.wrap(s.handleAdminStats))

	s.httpSrv = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(sd)
	}()
	s.log.Info("memserve listening", "addr", s.cfg.ListenAddr, "shardK", s.cfg.ShardK,
		"shardID", s.cfg.ShardID, "pruneD", s.cfg.Prune.D(), "signed", s.identity != nil,
		"paymentRequired", s.cfg.PaymentRequired)
	err := s.httpSrv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Handler returns the HTTP handler (for tests via httptest).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/seen", s.wrap(s.handleSeen))
	mux.HandleFunc("/v1/mined", s.wrap(s.handleMined))
	mux.HandleFunc("/v1/merklepath", s.wrap(s.handleMerklePath))
	mux.HandleFunc("/v1/utxo", s.wrap(s.handleUTXO))
	mux.HandleFunc("/v1/channel/open", s.wrap(s.handleChannelOpen))
	mux.HandleFunc("/v1/quote", s.wrap(s.handleQuote))
	mux.HandleFunc("/v1/paid/query", s.wrap(s.handlePaidQuery))
	mux.HandleFunc("/admin/stats", s.wrap(s.handleAdminStats))
	return mux
}

// IngestOnce drains the source once (used by tests to populate deterministically).
func (s *Server) IngestOnce() error {
	for {
		b, ok, err := s.src.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if _, err := s.ing.IngestBlock(b); err != nil {
			return err
		}
		s.tip.Store(b.Height)
		s.met.blocks.Add(1)
	}
	s.ready.Store(true)
	return nil
}

func (s *Server) ingestLoop(ctx context.Context) {
	var lastWarn time.Time
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Memory watchdog: if the heap exceeds MaxMemMB, PAUSE ingestion (do not fetch
		// the next block) until memory frees. This stops unbounded growth from taking the
		// host down; no block is dropped (we just don't advance). The store keeps serving.
		if s.cfg.MaxMemMB > 0 {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if int(ms.HeapAlloc/(1<<20)) >= s.cfg.MaxMemMB {
				if time.Since(lastWarn) > 30*time.Second {
					s.log.Warn("ingest paused: heap at memory limit",
						"heapMB", ms.HeapAlloc/(1<<20), "maxMemMB", s.cfg.MaxMemMB, "tip", s.tip.Load())
					lastWarn = time.Now()
				}
				s.ready.Store(true)
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.cfg.PollEvery):
				}
				continue
			}
		}
		b, ok, err := s.src.Next()
		if err != nil {
			s.log.Error("ingest", "err", err)
			time.Sleep(s.cfg.PollEvery)
			continue
		}
		if !ok {
			s.ready.Store(true) // caught up to tip
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.cfg.PollEvery):
			}
			continue
		}
		if _, err := s.ing.IngestBlock(b); err != nil {
			s.log.Error("ingest block", "height", b.Height, "err", err)
			continue
		}
		s.tip.Store(b.Height)
		s.met.blocks.Add(1)
		s.ready.Store(true) // serving once we have ingested data (and/or caught up)
	}
}

// --- middleware -------------------------------------------------------------

func (s *Server) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.met.requests.Add(1)
		defer func() {
			if rec := recover(); rec != nil {
				s.met.errors.Add(1)
				s.log.Error("panic", "path", r.URL.Path, "recover", fmt.Sprint(rec))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		if s.limiter != nil && !s.limiter.allow(clientIP(r)) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
		defer cancel()
		h(w, r.WithContext(ctx))
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- probes / metrics -------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") }

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ready.Load() {
		fmt.Fprintln(w, "ready")
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	st := s.store.Stats()
	ch, banned := s.paid.Counts()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "memserve_requests_total %d\n", s.met.requests.Load())
	fmt.Fprintf(w, "memserve_errors_total %d\n", s.met.errors.Load())
	fmt.Fprintf(w, "memserve_served_total %d\n", s.met.served.Load())
	fmt.Fprintf(w, "memserve_rejected_total %d\n", s.met.rejected.Load())
	fmt.Fprintf(w, "memserve_blocks_ingested %d\n", s.met.blocks.Load())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "memserve_heap_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintf(w, "memserve_max_mem_mb %d\n", s.cfg.MaxMemMB)
	fmt.Fprintf(w, "memserve_tip_height %d\n", s.tip.Load())
	fmt.Fprintf(w, "memserve_txindex %d\n", st.TxIndex)
	fmt.Fprintf(w, "memserve_utxo_live %d\n", st.UTXOLive)
	fmt.Fprintf(w, "memserve_utxo_spent_retained %d\n", st.UTXOSpent)
	fmt.Fprintf(w, "memserve_channels %d\n", ch)
	fmt.Fprintf(w, "memserve_channels_banned %d\n", banned)
	fmt.Fprintf(w, "memserve_revenue_satoshis %d\n", s.paid.Revenue())
}

// --- query handlers ---------------------------------------------------------

func (s *Server) txidParam(w http.ResponseWriter, r *http.Request) (store.Hash, bool) {
	h, ok := parseHash(r.URL.Query().Get("txid"))
	if !ok {
		http.Error(w, "bad txid", http.StatusBadRequest)
	}
	return h, ok
}

func (s *Server) handleSeen(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PaymentRequired {
		http.Error(w, "payment required: use /v1/paid/query", http.StatusPaymentRequired)
		return
	}
	txid, ok := s.txidParam(w, r)
	if !ok {
		return
	}
	res, _ := s.api.Seen(txid)
	s.met.served.Add(1)
	resp := SeenResponse{Seen: res.Seen, SeenTime: res.SeenTime, Tip: s.tip.Load()}
	if s.identity != nil {
		att, _ := s.identity.Attest(attest.Statement{Kind: attest.StmtSeen, TxID: txid, Flag: res.Seen, Tip: s.tip.Load()})
		j := EncodeAttestation(att)
		resp.Attestation = &j
	}
	writeJSON(w, resp)
}

func (s *Server) handleMined(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PaymentRequired {
		http.Error(w, "payment required: use /v1/paid/query", http.StatusPaymentRequired)
		return
	}
	txid, ok := s.txidParam(w, r)
	if !ok {
		return
	}
	res, _ := s.api.Mined(txid)
	s.met.served.Add(1)
	writeJSON(w, s.minedResp(txid, res))
}

func (s *Server) minedResp(txid store.Hash, res api.MinedResult) MinedResponse {
	resp := MinedResponse{Mined: res.Mined, Height: res.Height, BlockTime: res.BlockTime, Tip: s.tip.Load()}
	if res.Mined {
		resp.BlockHash = hexHash(res.BlockHash)
	}
	if s.identity != nil {
		att, _ := s.identity.Attest(attest.Statement{Kind: attest.StmtMined, TxID: txid,
			Flag: res.Mined, Height: res.Height, BlockHash: res.BlockHash, Tip: s.tip.Load()})
		j := EncodeAttestation(att)
		resp.Attestation = &j
	}
	return resp
}

func (s *Server) handleMerklePath(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PaymentRequired {
		http.Error(w, "payment required: use /v1/paid/query", http.StatusPaymentRequired)
		return
	}
	txid, ok := s.txidParam(w, r)
	if !ok {
		return
	}
	p, err := s.api.MerklePath(txid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.met.served.Add(1)
	writeJSON(w, MerklePathResponse{Proof: EncodeProof(p)})
}

func (s *Server) handleUTXO(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PaymentRequired {
		http.Error(w, "payment required: use /v1/paid/query", http.StatusPaymentRequired)
		return
	}
	txid, ok := s.txidParam(w, r)
	if !ok {
		return
	}
	var vout uint32
	fmt.Sscanf(r.URL.Query().Get("vout"), "%d", &vout)
	res, _ := s.api.UTXO(store.Outpoint{TxID: txid, Vout: vout})
	s.met.served.Add(1)
	writeJSON(w, s.utxoResp(store.Outpoint{TxID: txid, Vout: vout}, res))
}

func (s *Server) utxoResp(op store.Outpoint, res api.UTXOResult) UTXOResponse {
	resp := UTXOResponse{Status: res.Status.String(), Value: res.Value, SpentHeight: res.SpentHeight, Tip: s.tip.Load()}
	if s.identity != nil {
		att, _ := s.identity.Attest(attest.Statement{Kind: attest.StmtUTXO, TxID: op.TxID, Vout: op.Vout,
			Flag: res.Status == api.UTXOUnspent, Height: res.SpentHeight, Tip: s.tip.Load()})
		j := EncodeAttestation(att)
		resp.Attestation = &j
	}
	return resp
}

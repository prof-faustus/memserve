package server

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"

	"memserve/api"
	"memserve/attest"
	"memserve/crypto"
	"memserve/payment/channel"
	"memserve/store"
)

// openReq opens a payment channel.
type openReq struct {
	FundingTxID    string    `json:"fundingTxid"`
	Vout           uint32    `json:"vout"`
	Deposit        uint64    `json:"deposit"`
	ClientPub      string    `json:"clientPub"` // compressed pubkey hex
	Flat           bool      `json:"flat"`
	FlatPrice      uint64    `json:"flatPrice"`
	PerType        [4]uint64 `json:"perType"`
	SettleFee      uint64    `json:"settleFee"`
	FeeMode        uint8     `json:"feeMode"`
	N              uint64    `json:"n"`
	RefundLockTime uint32    `json:"refundLockTime"`
	SettleBefore   uint32    `json:"settleBefore"`
}

func (s *Server) handleChannelOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req openReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	fund, ok := parseHash(req.FundingTxID)
	if !ok {
		http.Error(w, "bad fundingTxid", http.StatusBadRequest)
		return
	}
	pubBytes, err := hex.DecodeString(req.ClientPub)
	if err != nil {
		http.Error(w, "bad clientPub", http.StatusBadRequest)
		return
	}
	pub, err := crypto.ParseCompressed(pubBytes)
	if err != nil {
		http.Error(w, "bad clientPub: "+err.Error(), http.StatusBadRequest)
		return
	}
	params := channel.Params{
		ChannelID:        channel.DeriveChannelID(fund, req.Vout),
		FundingTxID:      fund,
		FundingVout:      req.Vout,
		ServerScriptHash: serverPayee(s),
	}
	pricing := channel.Pricing{Flat: req.Flat, FlatPrice: req.FlatPrice, SettleFee: req.SettleFee,
		FeeMode: channel.FeeMode(req.FeeMode)}
	pricing.PerType = req.PerType
	_, err = s.paid.OpenChannel(channel.Config{
		Params: params, Deposit: req.Deposit, ClientPub: pub, Pricing: pricing,
		N: req.N, RefundLockTime: req.RefundLockTime, SettleBefore: req.SettleBefore,
	})
	if err != nil {
		s.met.rejected.Add(1)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"channelId": hexHash(params.ChannelID)})
}

func serverPayee(s *Server) store.Hash {
	// In production this is the operator's receiving scriptHash; derive a stable value here.
	var h store.Hash
	if s.identity != nil {
		copy(h[:], s.identity.Public().SerializeCompressed()[1:])
	}
	return h
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	ch, ok := parseHash(r.URL.Query().Get("channel"))
	if !ok {
		http.Error(w, "bad channel", http.StatusBadRequest)
		return
	}
	qt, err := strconv.Atoi(r.URL.Query().Get("type"))
	if err != nil || qt < 0 || qt > 3 {
		http.Error(w, "bad type (0=seen,1=mined,2=merklepath,3=utxo)", http.StatusBadRequest)
		return
	}
	cum, err := s.paid.Quote(ch, channel.QueryType(qt))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]uint64{"cumulative": cum})
}

// paidReq is a prepaid query: a commitment plus the query.
type paidReq struct {
	Channel   string `json:"channel"`
	Type      uint8  `json:"type"` // 0=seen 1=mined 2=merklepath 3=utxo
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	CumAmount uint64 `json:"cumAmount"`
	Sig       string `json:"sig"` // 64-byte hex
}

func (s *Server) handlePaidQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req paidReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ch, ok := parseHash(req.Channel)
	if !ok {
		http.Error(w, "bad channel", http.StatusBadRequest)
		return
	}
	txid, ok := parseHash(req.TxID)
	if !ok {
		http.Error(w, "bad txid", http.StatusBadRequest)
		return
	}
	sigBytes, err := hex.DecodeString(req.Sig)
	if err != nil {
		http.Error(w, "bad sig", http.StatusBadRequest)
		return
	}
	sig, err := crypto.ParseSignature(sigBytes)
	if err != nil {
		http.Error(w, "bad sig: "+err.Error(), http.StatusBadRequest)
		return
	}
	commit := &channel.Commitment{CumAmount: req.CumAmount, Sig: sig}

	switch req.Type {
	case 0:
		res, err := s.paid.Seen(ch, commit, txid)
		if s.payErr(w, err) {
			return
		}
		s.met.served.Add(1)
		resp := SeenResponse{Seen: res.Seen, SeenTime: res.SeenTime, Tip: s.tip.Load()}
		s.attachSeenAtt(&resp, txid, res)
		writeJSON(w, resp)
	case 1:
		res, err := s.paid.Mined(ch, commit, txid)
		if s.payErr(w, err) {
			return
		}
		s.met.served.Add(1)
		writeJSON(w, s.minedResp(txid, res))
	case 2:
		p, err := s.paid.MerklePath(ch, commit, txid)
		if s.payErr(w, err) {
			return
		}
		s.met.served.Add(1)
		writeJSON(w, MerklePathResponse{Proof: EncodeProof(p)})
	case 3:
		op := store.Outpoint{TxID: txid, Vout: req.Vout}
		res, err := s.paid.UTXO(ch, commit, op)
		if s.payErr(w, err) {
			return
		}
		s.met.served.Add(1)
		writeJSON(w, s.utxoResp(op, res))
	default:
		http.Error(w, "bad type", http.StatusBadRequest)
	}
}

func (s *Server) attachSeenAtt(resp *SeenResponse, txid store.Hash, res api.SeenResult) {
	if s.identity == nil {
		return
	}
	att, _ := s.identity.Attest(attest.Statement{Kind: attest.StmtSeen, TxID: txid, Flag: res.Seen, Tip: s.tip.Load()})
	j := EncodeAttestation(att)
	resp.Attestation = &j
}

// payErr writes the right status for a payment/abuse error; returns true if it handled one.
func (s *Server) payErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	s.met.rejected.Add(1)
	http.Error(w, err.Error(), http.StatusPaymentRequired)
	return true
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AdminToken == "" || r.Header.Get("Authorization") != "Bearer "+s.cfg.AdminToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := s.store.Stats()
	ch, banned := s.paid.Counts()
	writeJSON(w, map[string]any{
		"tip":               s.tip.Load(),
		"ready":             s.ready.Load(),
		"pruneD":            s.cfg.Prune.D(),
		"reorgHorizon":      s.cfg.Prune.ReorgHorizon,
		"txindex":           st.TxIndex,
		"utxoLive":          st.UTXOLive,
		"utxoSpentRetained": st.UTXOSpent,
		"channels":          ch,
		"channelsBanned":    banned,
		"revenueSatoshis":   s.paid.Revenue(),
		"operatorPub":       s.OperatorPubHex(),
	})
}

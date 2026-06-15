// Package payment wires the BSV payment channel (payment/channel) to the lookup API
// (api): every query is PREPAID then served. A PaidServer verifies the client's
// commitment for the next access before answering, so the server can never serve an
// unpaid access (DESIGN.md §10). Channels are per-shard and held here in a registry.
// BSV only.
package payment

import (
	"errors"
	"sync"

	"memserve/api"
	"memserve/payment/channel"
	"memserve/proof"
	"memserve/store"
)

// Errors.
var (
	ErrNoChannel       = errors.New("payment: unknown channel")
	ErrChannelBanned   = errors.New("payment: channel banned (abuse budget exceeded)")
	ErrDepositTooSmall = errors.New("payment: deposit below MinDeposit policy")
	ErrTooManyChannels = errors.New("payment: MaxChannels reached")
)

// PaidServer is a payment-gated lookup server for one shard, with abuse defenses
// (DESIGN.md §15).
type PaidServer struct {
	api      *api.Server
	policy   Policy
	notifier Notifier

	mu       sync.RWMutex
	channels map[store.Hash]*channel.Channel
	bad      map[store.Hash]int  // per-channel invalid-commitment count
	banned   map[store.Hash]bool // channels cut off for abuse
}

// New builds a PaidServer with the default abuse policy and no-op notifier.
func New(a *api.Server) *PaidServer {
	return NewWithPolicy(a, DefaultPolicy(), nil)
}

// NewWithPolicy builds a PaidServer with an explicit abuse policy and operator notifier
// (the "path for calls to the user of the system"). A nil notifier discards alerts.
func NewWithPolicy(a *api.Server, p Policy, n Notifier) *PaidServer {
	if n == nil {
		n = nopNotifier{}
	}
	return &PaidServer{
		api:      a,
		policy:   p,
		notifier: n,
		channels: make(map[store.Hash]*channel.Channel),
		bad:      make(map[store.Hash]int),
		banned:   make(map[store.Hash]bool),
	}
}

// OpenChannel opens and registers a channel, enforcing the abuse policy: a deposit below
// MinDeposit is rejected, and MaxChannels caps concurrent channels (defeating open-flood).
func (s *PaidServer) OpenChannel(cfg channel.Config) (*channel.Channel, error) {
	if s.policy.MinDeposit > 0 && cfg.Deposit < s.policy.MinDeposit {
		s.notifier.Notify(Alert{Kind: AlertOpenRejected, ChannelID: cfg.Params.ChannelID,
			Detail: "deposit below MinDeposit"})
		return nil, ErrDepositTooSmall
	}
	s.mu.Lock()
	if s.policy.MaxChannels > 0 && len(s.channels) >= s.policy.MaxChannels {
		count := len(s.channels)
		s.mu.Unlock()
		s.notifier.Notify(Alert{Kind: AlertOpenFlood, ChannelID: cfg.Params.ChannelID,
			Detail: "MaxChannels reached", Count: count})
		return nil, ErrTooManyChannels
	}
	s.mu.Unlock()

	ch, err := channel.Open(cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.channels[cfg.Params.ChannelID] = ch
	s.mu.Unlock()
	return ch, nil
}

func (s *PaidServer) lookup(id store.Hash) (*channel.Channel, error) {
	s.mu.RLock()
	ch, ok := s.channels[id]
	ban := s.banned[id]
	s.mu.RUnlock()
	if ban {
		return nil, ErrChannelBanned
	}
	if !ok {
		return nil, ErrNoChannel
	}
	return ch, nil
}

// recordBad increments a channel's invalid-attempt count and bans it once it exceeds the
// budget, alerting the operator. This bounds wasted verify work per (funded) channel.
func (s *PaidServer) recordBad(id store.Hash) {
	if s.policy.MaxBadAttempts <= 0 {
		return
	}
	s.mu.Lock()
	s.bad[id]++
	n := s.bad[id]
	justBanned := false
	if n >= s.policy.MaxBadAttempts && !s.banned[id] {
		s.banned[id] = true
		justBanned = true
	}
	s.mu.Unlock()
	if justBanned {
		s.notifier.Notify(Alert{Kind: AlertChannelBanned, ChannelID: id,
			Detail: "bad-attempt budget exceeded", Count: n})
	}
}

// Quote returns the cumulative amount the client must sign for the next access of q on
// the given channel.
func (s *PaidServer) Quote(channelID store.Hash, q channel.QueryType) (uint64, error) {
	ch, err := s.lookup(channelID)
	if err != nil {
		return 0, err
	}
	return ch.Quote(q), nil
}

// authorize verifies and books the prepayment before serving. Cheap structural checks
// happen inside channel.Authorize BEFORE the expensive secp256k1 verify; any invalid
// attempt is counted toward the channel's abuse budget (banning floods).
func (s *PaidServer) authorize(channelID store.Hash, c *channel.Commitment, q channel.QueryType) error {
	ch, err := s.lookup(channelID)
	if err != nil {
		return err
	}
	if err := ch.Authorize(c, q); err != nil {
		// Invalid commitment (bad sig / underpaid / over deposit): charge the abuse budget.
		s.recordBad(channelID)
		return err
	}
	return nil
}

// Seen: prepay, then answer "seen?".
func (s *PaidServer) Seen(channelID store.Hash, c *channel.Commitment, txid store.Hash) (api.SeenResult, error) {
	if err := s.authorize(channelID, c, channel.QSeen); err != nil {
		return api.SeenResult{}, err
	}
	return s.api.Seen(txid)
}

// Mined: prepay, then answer "mined? when?".
func (s *PaidServer) Mined(channelID store.Hash, c *channel.Commitment, txid store.Hash) (api.MinedResult, error) {
	if err := s.authorize(channelID, c, channel.QMined); err != nil {
		return api.MinedResult{}, err
	}
	return s.api.Mined(txid)
}

// MerklePath: prepay, then serve the inclusion proof.
func (s *PaidServer) MerklePath(channelID store.Hash, c *channel.Commitment, txid store.Hash) (proof.Proof, error) {
	if err := s.authorize(channelID, c, channel.QMerklePath); err != nil {
		return proof.Proof{}, err
	}
	return s.api.MerklePath(txid)
}

// Revenue returns the total prepaid (cumulative) across all channels — the operator's
// earned/earning income, for the admin/metrics view (miner value-add).
func (s *PaidServer) Revenue() uint64 {
	s.mu.RLock()
	chs := make([]*channel.Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		chs = append(chs, ch)
	}
	s.mu.RUnlock()
	var total uint64
	for _, ch := range chs {
		total += ch.Snapshot().CumPaid
	}
	return total
}

// Counts returns the number of open channels and banned channels (admin/metrics).
func (s *PaidServer) Counts() (channels, banned int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.channels), len(s.banned)
}

// UTXO: prepay, then answer the UTXO status.
func (s *PaidServer) UTXO(channelID store.Hash, c *channel.Commitment, op store.Outpoint) (api.UTXOResult, error) {
	if err := s.authorize(channelID, c, channel.QUTXO); err != nil {
		return api.UTXOResult{}, err
	}
	return s.api.UTXO(op)
}

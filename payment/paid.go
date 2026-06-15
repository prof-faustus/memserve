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

// ErrNoChannel is returned for an unknown channel id.
var ErrNoChannel = errors.New("payment: unknown channel")

// PaidServer is a payment-gated lookup server for one shard.
type PaidServer struct {
	api *api.Server

	mu       sync.RWMutex
	channels map[store.Hash]*channel.Channel
}

// New builds a PaidServer over a query API.
func New(a *api.Server) *PaidServer {
	return &PaidServer{api: a, channels: make(map[store.Hash]*channel.Channel)}
}

// OpenChannel opens and registers a channel; its id is cfg.Params.ChannelID.
func (s *PaidServer) OpenChannel(cfg channel.Config) (*channel.Channel, error) {
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
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNoChannel
	}
	return ch, nil
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

// authorize verifies and books the prepayment before serving.
func (s *PaidServer) authorize(channelID store.Hash, c *channel.Commitment, q channel.QueryType) error {
	ch, err := s.lookup(channelID)
	if err != nil {
		return err
	}
	return ch.Authorize(c, q)
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

// UTXO: prepay, then answer the UTXO status.
func (s *PaidServer) UTXO(channelID store.Hash, c *channel.Commitment, op store.Outpoint) (api.UTXOResult, error) {
	if err := s.authorize(channelID, c, channel.QUTXO); err != nil {
		return api.UTXOResult{}, err
	}
	return s.api.UTXO(op)
}

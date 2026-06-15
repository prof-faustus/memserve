package payment

import "memserve/store"

// Abuse / griefing defense (DESIGN.md §15). Two vectors are addressed:
//
//  1. Channel-open flood -> server state/memory exhaustion. Defeated by requiring a
//     funded, confirmed deposit >= MinDeposit to allocate channel state, plus a global
//     MaxChannels cap. Opening N channels costs N on-chain funded deposits (real
//     capital locked + miner fees) — the attacker pays to attack.
//  2. Invalid-commitment flood -> asymmetric secp256k1-verify-cost DoS (a bad signature
//     is cheap to send, ~ms to verify). Defeated by (a) cheap O(1) structural checks
//     BEFORE the expensive verify (done in channel.Authorize: amount/deposit/low-S form),
//     and (b) a per-channel bad-attempt budget: a channel exceeding MaxBadAttempts is
//     banned, bounding wasted verify work per locked deposit. The attacker forfeits the
//     open cost and gains nothing.
//
// Net economics: every channel costs the attacker a real on-chain deposit; valid queries
// are prepaid (the server profits); invalid floods are filtered cheaply, throttled, and
// the channel is cut after a bounded number of attempts. The attack loses the attacker
// money and cannot drain the server.

// Policy parameterizes the abuse defenses. Zero values disable a given control.
type Policy struct {
	MinDeposit     uint64 // reject OpenChannel below this (0 = no minimum)
	MaxChannels    int    // global cap on concurrent channels (0 = unlimited)
	MaxBadAttempts int    // per-channel invalid-commitment budget before ban (0 = unlimited)
}

// DefaultPolicy is protective by default for the verify-flood vector; the operator should
// also set MinDeposit and MaxChannels for their deployment (capital/capacity dependent).
func DefaultPolicy() Policy {
	return Policy{MinDeposit: 0, MaxChannels: 0, MaxBadAttempts: 64}
}

// AlertKind classifies an operator alert.
type AlertKind uint8

const (
	AlertChannelBanned AlertKind = iota // a channel hit its bad-attempt budget and was banned
	AlertOpenRejected                   // an OpenChannel was rejected (deposit too small)
	AlertOpenFlood                      // OpenChannel rejected because MaxChannels reached
)

func (k AlertKind) String() string {
	switch k {
	case AlertChannelBanned:
		return "channel-banned"
	case AlertOpenRejected:
		return "open-rejected"
	case AlertOpenFlood:
		return "open-flood"
	}
	return "unknown"
}

// Alert is delivered to the operator when a defense trips. This is the "path for calls to
// the user of the system": wire a Notifier to page/log/throttle upstream.
type Alert struct {
	Kind      AlertKind
	ChannelID store.Hash
	Detail    string
	Count     int // e.g. bad-attempt count, or current channel count
}

// Notifier receives operator alerts. Implementations must be safe for concurrent use and
// must not block (do slow work asynchronously).
type Notifier interface {
	Notify(Alert)
}

// NotifierFunc adapts a function to Notifier.
type NotifierFunc func(Alert)

// Notify implements Notifier.
func (f NotifierFunc) Notify(a Alert) { f(a) }

// nopNotifier discards alerts (default).
type nopNotifier struct{}

func (nopNotifier) Notify(Alert) {}

package bsvtx

import "errors"

// ChannelTx builds the on-chain transactions of a BSV payment channel from the funding
// 2-of-2 output. All spends use the FORKID sighash over the 2-of-2 redeem script.
//
//	Funding:    client's coins -> 2-of-2(client,server)   [client builds & broadcasts]
//	Commitment: 2-of-2 -> {server: toServer, client: rest-fee}  [client signs each increment]
//	Settlement: a fully-signed commitment (client sig + server sig) the server broadcasts
//	Refund:     2-of-2 -> client (full deposit-fee), nLockTime=x  [server pre-signs at open]
type ChannelTx struct {
	FundingOut   OutPoint
	FundingValue uint64
	ClientPub    []byte // compressed (33)
	ServerPub    []byte // compressed (33)
	Fee          uint64 // miner fee reserved for the settling tx
}

// ErrAmount is returned when payouts don't fit the funded value.
var ErrAmount = errors.New("bsvtx: payout exceeds funded value minus fee")

// Redeem returns the funding output's locking script (bare 2-of-2, client key first).
func (c ChannelTx) Redeem() []byte { return Multisig2of2(c.ClientPub, c.ServerPub) }

// FundingOutput is the TxOut the client's funding transaction must create.
func (c ChannelTx) FundingOutput() TxOut {
	return TxOut{Value: c.FundingValue, ScriptPubKey: c.Redeem()}
}

// CommitmentTx spends the funding output paying the server `toServer` and the remainder
// (minus fee) back to the client. Outputs are in a fixed order [server, client].
func (c ChannelTx) CommitmentTx(toServer uint64) (*Tx, error) {
	if toServer+c.Fee > c.FundingValue {
		return nil, ErrAmount
	}
	toClient := c.FundingValue - toServer - c.Fee
	return &Tx{
		Version:  2,
		Inputs:   []TxIn{{PrevOut: c.FundingOut, Sequence: FinalSequence}},
		Outputs:  []TxOut{{Value: toServer, ScriptPubKey: P2PK(c.ServerPub)}, {Value: toClient, ScriptPubKey: P2PK(c.ClientPub)}},
		LockTime: 0,
	}, nil
}

// CommitmentSighash is the FORKID sighash the client signs to authorize paying the server
// `toServer` (the channel's cumulative amount). Deterministic for given params.
func (c ChannelTx) CommitmentSighash(toServer uint64) ([]byte, error) {
	tx, err := c.CommitmentTx(toServer)
	if err != nil {
		return nil, err
	}
	return tx.SighashForkID(0, c.Redeem(), c.FundingValue, SighashAllFork), nil
}

// RefundTx returns the full deposit (minus fee) to the client, spendable only at/after
// nLockTime — the client's safety net if the server never settles.
func (c ChannelTx) RefundTx(lockTime uint32) *Tx {
	return &Tx{
		Version:  2,
		Inputs:   []TxIn{{PrevOut: c.FundingOut, Sequence: EnableLockTimeSequence}},
		Outputs:  []TxOut{{Value: c.FundingValue - c.Fee, ScriptPubKey: P2PK(c.ClientPub)}},
		LockTime: lockTime,
	}
}

// RefundSighash is the sighash both parties sign for the refund tx.
func (c ChannelTx) RefundSighash(lockTime uint32) []byte {
	return c.RefundTx(lockTime).SighashForkID(0, c.Redeem(), c.FundingValue, SighashAllFork)
}

// Finalize2of2 installs the unlocking script (OP_0 clientSig serverSig) on input 0,
// making the tx broadcastable. Signature order matches the redeem's key order.
func Finalize2of2(tx *Tx, clientSigWithType, serverSigWithType []byte) {
	tx.Inputs[0].ScriptSig = ScriptSig2of2(clientSigWithType, serverSigWithType)
}

# BSV testnet broadcast — channel transaction acceptance

This is the consensus-acceptance test for the `bsvtx` layer (DESIGN §10.6): build the real
funding / commitment / settlement / refund transactions and broadcast them to BSV
**testnet**. If testnet accepts them, the FORKID sighash + scripts are consensus-correct;
if it rejects one, the error pins exactly what to fix.

Run on a machine with internet (the VM). `SEED` is any 32-byte hex you control.

```sh
SEED=$(openssl rand -hex 32)

# 1) Get the client's testnet address and fund it from a faucet (e.g. a BSV testnet faucet).
go run ./cmd/channeltestnet -client-seed $SEED -mode address
#   -> client address: m...    (send testnet sats here; note the funding UTXO txid:vout:value)

# 2) After the faucet tx confirms, lock a deposit into the 2-of-2 funding output:
go run ./cmd/channeltestnet -client-seed $SEED -mode fund \
   -utxo-txid <faucet_txid> -utxo-vout <n> -utxo-value <sats> \
   -deposit 50000 -fee 500 -broadcast
#   -> FUNDING txid + "use for settle: -funding-txid <id> -funding-vout 0 -funding-value 50000"

# 3) After the funding tx confirms, broadcast a SETTLEMENT (2-of-2 spend; pay the server,
#    change to the client) and print the nLockTime REFUND:
go run ./cmd/channeltestnet -client-seed $SEED -mode settle \
   -funding-txid <funding_txid> -funding-vout 0 -funding-value 50000 \
   -pay 4000 -fee 500 -locktime 1600000 -broadcast
```

Verify each txid on a testnet explorer (e.g. WhatsOnChain testnet). Notes:

- Default broadcast endpoint is WhatsOnChain testnet (`-api` to override / use your node).
- Wait for confirmations between steps 2 and 3 — the **confirmed-funding** rule (DESIGN
  §10.6) avoids the Spillman malleability window.
- Omit `-broadcast` to print the raw signed hex without sending (offline review / manual
  broadcast).
- Keys are derived deterministically from the seed; the server key is derived from the
  client key unless `-server-seed` is given.
```

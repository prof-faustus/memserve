// Command channeltestnet builds and broadcasts the REAL BSV payment-channel transactions
// on testnet — the consensus-acceptance test for the bsvtx layer (DESIGN §10.6).
//
// Flow (run the modes in order; fund a faucet, wait for confirmations between steps):
//
//	# 1) print the client address to fund from a testnet faucet:
//	go run ./cmd/channeltestnet -client-seed <hex32> -mode address
//
//	# 2) once funded, build+broadcast the FUNDING tx (spends the faucet P2PKH utxo into a
//	#    2-of-2(client,server) output of -deposit sats):
//	go run ./cmd/channeltestnet -client-seed <hex32> -mode fund \
//	   -utxo-txid <id> -utxo-vout <n> -utxo-value <sats> -deposit <sats> -broadcast
//
//	# 3) after the funding tx confirms, build+broadcast the SETTLEMENT (2-of-2 spend paying
//	#    the server -pay sats, change to the client), and print the nLockTime REFUND:
//	go run ./cmd/channeltestnet -client-seed <hex32> -mode settle \
//	   -funding-txid <id> -funding-vout 0 -funding-value <deposit> -pay <sats> -broadcast
//
// I cannot run this from the build sandbox (no testnet network/coins); run it on the VM.
// If testnet rejects a tx, the rejection pins exactly what to fix in bsvtx. BSV only.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"memserve/bsvtx"
	"memserve/commitment"
	"memserve/crypto"
)

func main() {
	clientSeed := flag.String("client-seed", "", "32-byte hex seed for the client key (required)")
	serverSeed := flag.String("server-seed", "", "32-byte hex seed for the server key (default: derived from client)")
	mode := flag.String("mode", "address", "address | fund | settle")
	net := flag.String("net", "test", "test | main (address version)")
	fee := flag.Uint64("fee", 500, "miner fee (sats)")
	deposit := flag.Uint64("deposit", 0, "fund: sats to lock in the 2-of-2")
	utxoTxid := flag.String("utxo-txid", "", "fund: funded faucet UTXO txid (display/explorer hex)")
	utxoVout := flag.Uint("utxo-vout", 0, "fund: funded UTXO vout")
	utxoValue := flag.Uint64("utxo-value", 0, "fund: funded UTXO value (sats)")
	fundingTxid := flag.String("funding-txid", "", "settle: funding tx id (display hex)")
	fundingVout := flag.Uint("funding-vout", 0, "settle: funding output index")
	fundingValue := flag.Uint64("funding-value", 0, "settle: funding output value (sats)")
	pay := flag.Uint64("pay", 0, "settle: cumulative sats to pay the server")
	lockTime := flag.Uint("locktime", 0, "refund nLockTime (block height or unix time)")
	api := flag.String("api", "https://api.whatsonchain.com/v1/bsv/test/tx/raw", "broadcast endpoint")
	utxoAPI := flag.String("utxo-api", "https://api.whatsonchain.com/v1/bsv/test/address", "address base for UTXO lookup ({base}/{addr}/unspent)")
	statusAPI := flag.String("status-api", "https://api.whatsonchain.com/v1/bsv/test/tx/hash", "tx-status base ({base}/{txid})")
	statusTxid := flag.String("txid", "", "status: tx id to check confirmations for")
	broadcast := flag.Bool("broadcast", false, "POST the tx to -api")
	flag.Parse()

	if *clientSeed == "" {
		fmt.Println("error: -client-seed <hex32> is required")
		os.Exit(2)
	}
	ver := byte(bsvtx.AddrTestP2PKH)
	if *net == "main" {
		ver = bsvtx.AddrMainP2PKH
	}
	clientPriv := mustKey(*clientSeed)
	srvSeed := *serverSeed
	if srvSeed == "" {
		h := commitment.DoubleSHA256(append([]byte("server-of:"), clientPriv.Public().SerializeCompressed()...))
		srvSeed = hex.EncodeToString(h[:])
	}
	serverPriv := mustKey(srvSeed)
	clientPub := clientPriv.Public().SerializeCompressed()
	serverPub := serverPriv.Public().SerializeCompressed()

	switch *mode {
	case "address":
		fmt.Printf("client address (FUND THIS from a testnet faucet): %s\n", bsvtx.AddressFromPubKey(clientPub, ver))
		fmt.Printf("client pub: %s\n", hex.EncodeToString(clientPub))
		fmt.Printf("server pub: %s\n", hex.EncodeToString(serverPub))
		fmt.Printf("server seed (keep): %s\n", srvSeed)
	case "utxo":
		addr := bsvtx.AddressFromPubKey(clientPub, ver)
		us, err := fetchUTXOs(*utxoAPI, addr)
		if err != nil {
			fmt.Println("utxo fetch failed:", err)
			os.Exit(1)
		}
		fmt.Printf("UTXOs for %s (%d):\n", addr, len(us))
		for _, u := range us {
			fmt.Printf("  %s:%d  %d sats  (height %d)\n", u.TxID, u.Vout, u.Value, u.Height)
		}
	case "status":
		if *statusTxid == "" {
			fmt.Println("error: status needs -txid")
			os.Exit(2)
		}
		conf, height, err := fetchStatus(*statusAPI, *statusTxid)
		if err != nil {
			fmt.Println("status fetch failed:", err)
			os.Exit(1)
		}
		fmt.Printf("tx %s: confirmations=%d blockHeight=%d\n", *statusTxid, conf, height)
	case "fund":
		if *deposit == 0 {
			fmt.Println("error: fund needs -deposit")
			os.Exit(2)
		}
		uTxid, uVout, uValue := *utxoTxid, uint32(*utxoVout), *utxoValue
		if uTxid == "" { // auto-discover the funding UTXO from the explorer
			addr := bsvtx.AddressFromPubKey(clientPub, ver)
			best, err := pickUTXO(*utxoAPI, addr, *deposit+*fee)
			if err != nil {
				fmt.Printf("auto-fetch UTXO for %s failed: %v\n", addr, err)
				os.Exit(1)
			}
			uTxid, uVout, uValue = best.TxID, best.Vout, best.Value
			fmt.Printf("using UTXO %s:%d (%d sats)\n", uTxid, uVout, uValue)
		}
		if uValue < *deposit+*fee {
			fmt.Printf("error: UTXO value %d < deposit+fee %d\n", uValue, *deposit+*fee)
			os.Exit(2)
		}
		tx := buildFunding(clientPriv, clientPub, serverPub, uTxid, uVout, uValue, *deposit, *fee)
		emit("FUNDING", tx, *broadcast, *api)
		fmt.Printf("=> use for settle: -funding-txid %s -funding-vout 0 -funding-value %d\n",
			hex.EncodeToString(rev(tx.TxID())), *deposit)
	case "settle":
		if *fundingTxid == "" || *fundingValue == 0 {
			fmt.Println("error: settle needs -funding-txid -funding-vout -funding-value -pay")
			os.Exit(2)
		}
		c := channelTx(*fundingTxid, uint32(*fundingVout), *fundingValue, clientPub, serverPub, *fee)
		settle := buildSettlement(c, clientPriv, serverPriv, *pay)
		emit("SETTLEMENT", settle, *broadcast, *api)
		lt := uint32(*lockTime)
		refund := buildRefund(c, clientPriv, serverPriv, lt)
		fmt.Printf("\nREFUND (nLockTime=%d, client safety net — broadcast only if the server never settles):\n", lt)
		fmt.Printf("  txid=%s\n  raw=%s\n", hex.EncodeToString(rev(refund.TxID())), hex.EncodeToString(refund.Serialize()))
	default:
		fmt.Println("unknown -mode")
		os.Exit(2)
	}
}

func mustKey(seedHex string) *crypto.PrivateKey {
	b, err := hex.DecodeString(seedHex)
	if err != nil || len(b) != 32 {
		fmt.Println("error: seed must be 32-byte hex")
		os.Exit(2)
	}
	k, _ := crypto.NewPrivateKey(b)
	return k
}

// rev reverses internal txid bytes to the display/explorer order.
func rev(h [32]byte) []byte {
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		out[i] = h[31-i]
	}
	return out
}

// dispToInternal parses a display (big-endian) txid hex into internal byte order.
func dispToInternal(s string) [32]byte {
	b, _ := hex.DecodeString(s)
	var h [32]byte
	for i := 0; i < 32 && i < len(b); i++ {
		h[i] = b[len(b)-1-i]
	}
	return h
}

func buildFunding(clientPriv *crypto.PrivateKey, clientPub, serverPub []byte, utxoTxid string, vout uint32, utxoValue, deposit, fee uint64) *bsvtx.Tx {
	change := utxoValue - deposit - fee
	tx := &bsvtx.Tx{Version: 2,
		Inputs: []bsvtx.TxIn{{PrevOut: bsvtx.OutPoint{Hash: dispToInternal(utxoTxid), Index: vout}, Sequence: bsvtx.FinalSequence}},
		Outputs: []bsvtx.TxOut{
			{Value: deposit, ScriptPubKey: bsvtx.Multisig2of2(clientPub, serverPub)},
			{Value: change, ScriptPubKey: bsvtx.P2PKHFromPub(clientPub)},
		},
	}
	scriptCode := bsvtx.P2PKHFromPub(clientPub) // the faucet UTXO is P2PKH to the client
	sig, err := bsvtx.SignInput(clientPriv, tx, 0, scriptCode, utxoValue, bsvtx.SighashAllFork)
	if err != nil {
		fmt.Println("sign funding:", err)
		os.Exit(1)
	}
	tx.Inputs[0].ScriptSig = bsvtx.ScriptSigP2PKH(sig, clientPub)
	return tx
}

func channelTx(fundingTxidDisp string, vout uint32, value uint64, clientPub, serverPub []byte, fee uint64) bsvtx.ChannelTx {
	return bsvtx.ChannelTx{
		FundingOut:   bsvtx.OutPoint{Hash: dispToInternal(fundingTxidDisp), Index: vout},
		FundingValue: value, ClientPub: clientPub, ServerPub: serverPub, Fee: fee,
	}
}

func buildSettlement(c bsvtx.ChannelTx, clientPriv, serverPriv *crypto.PrivateKey, pay uint64) *bsvtx.Tx {
	tx, err := c.CommitmentTx(pay)
	if err != nil {
		fmt.Println("commitment:", err)
		os.Exit(1)
	}
	clientSig, _ := bsvtx.SignInput(clientPriv, tx, 0, c.Redeem(), c.FundingValue, bsvtx.SighashAllFork)
	serverSig, _ := bsvtx.SignInput(serverPriv, tx, 0, c.Redeem(), c.FundingValue, bsvtx.SighashAllFork)
	bsvtx.Finalize2of2(tx, clientSig, serverSig)
	return tx
}

func buildRefund(c bsvtx.ChannelTx, clientPriv, serverPriv *crypto.PrivateKey, lockTime uint32) *bsvtx.Tx {
	tx := c.RefundTx(lockTime)
	clientSig, _ := bsvtx.SignInput(clientPriv, tx, 0, c.Redeem(), c.FundingValue, bsvtx.SighashAllFork)
	serverSig, _ := bsvtx.SignInput(serverPriv, tx, 0, c.Redeem(), c.FundingValue, bsvtx.SighashAllFork)
	bsvtx.Finalize2of2(tx, clientSig, serverSig)
	return tx
}

func emit(label string, tx *bsvtx.Tx, broadcast bool, api string) {
	raw := tx.Serialize()
	fmt.Printf("%s txid=%s\n  raw=%s\n", label, hex.EncodeToString(rev(tx.TxID())), hex.EncodeToString(raw))
	if !broadcast {
		fmt.Printf("  (not broadcast; add -broadcast to POST to %s)\n", api)
		return
	}
	id, err := postRaw(api, hex.EncodeToString(raw))
	if err != nil {
		fmt.Printf("  BROADCAST FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  BROADCAST OK -> %s\n", id)
}

type utxoRef struct {
	TxID   string
	Vout   uint32
	Value  uint64
	Height uint32
}

// fetchUTXOs lists the unspent outputs for an address (WhatsOnChain-style:
// {base}/{addr}/unspent -> [{height,tx_pos,tx_hash,value}]).
func fetchUTXOs(apiBase, addr string) ([]utxoRef, error) {
	url := fmt.Sprintf("%s/%s/unspent", strings.TrimRight(apiBase, "/"), addr)
	var raw []struct {
		Height uint32 `json:"height"`
		TxPos  uint32 `json:"tx_pos"`
		TxHash string `json:"tx_hash"`
		Value  uint64 `json:"value"`
	}
	if err := getJSON(url, &raw); err != nil {
		return nil, err
	}
	out := make([]utxoRef, len(raw))
	for i, r := range raw {
		out[i] = utxoRef{TxID: r.TxHash, Vout: r.TxPos, Value: r.Value, Height: r.Height}
	}
	return out, nil
}

// pickUTXO returns the largest single unspent output that covers `need` sats.
func pickUTXO(apiBase, addr string, need uint64) (utxoRef, error) {
	us, err := fetchUTXOs(apiBase, addr)
	if err != nil {
		return utxoRef{}, err
	}
	var best utxoRef
	for _, u := range us {
		if u.Value >= need && u.Value > best.Value {
			best = u
		}
	}
	if best.TxID == "" {
		return utxoRef{}, fmt.Errorf("no single UTXO >= %d sats among %d unspent", need, len(us))
	}
	return best, nil
}

// fetchStatus returns a tx's confirmations and block height (WhatsOnChain-style).
func fetchStatus(apiBase, txid string) (confirmations, blockHeight int, err error) {
	url := fmt.Sprintf("%s/%s", strings.TrimRight(apiBase, "/"), txid)
	var raw struct {
		Confirmations int `json:"confirmations"`
		BlockHeight   int `json:"blockheight"`
	}
	if err := getJSON(url, &raw); err != nil {
		return 0, 0, err
	}
	return raw.Confirmations, raw.BlockHeight, nil
}

func getJSON(url string, dst any) error {
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return json.Unmarshal(b, dst)
}

func postRaw(api, txhex string) (string, error) {
	body, _ := json.Marshal(map[string]string{"txhex": txhex})
	req, _ := http.NewRequest(http.MethodPost, api, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return string(bytes.Trim(b, "\"\n ")), nil
}

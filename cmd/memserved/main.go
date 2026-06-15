// Command memserved is the MemServe production daemon (DESIGN.md §17): a commercial-grade
// HTTP/JSON shard service that ingests from Teranode (real HTTP source, or the built-in
// mock for a demo) and serves Seen/Mined/MerklePath/UTXO with payment metering, signed
// attestations, health/metrics, rate limiting, and graceful shutdown.
//
// Miner sidecar: point -teranode at your node's asset server; the daemon ingests your
// chain and monetizes serving via payment channels (see /admin/stats revenueSatoshis).
//
//	# demo against the built-in mock source:
//	go run ./cmd/memserved -mock -addr :8080
//	# production against a Teranode asset server:
//	go run ./cmd/memserved -teranode http://teranode:8090 -addr :8080 -operator-seed <hex32> -admin-token <tok>
//
// BSV/Teranode only.
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"memserve/payment"
	"memserve/prune"
	"memserve/server"
	"memserve/teranode"
	"memserve/teranode/httpsource"
)

func paymentPolicy(minDeposit uint64, maxChannels int) payment.Policy {
	p := payment.DefaultPolicy()
	p.MinDeposit = minDeposit
	p.MaxChannels = maxChannels
	return p
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	teranodeURL := flag.String("teranode", "", "Teranode asset/data server base URL")
	mock := flag.Bool("mock", false, "use the built-in mock Teranode source (demo)")
	shardK := flag.Uint("shard-k", 0, "hash-prefix shard width (0 = single shard)")
	shardID := flag.Uint("shard-id", 0, "this server's shard id")
	reorg := flag.Uint("reorg-horizon", 18, "prune reorg-horizon correctness floor (blocks)")
	recency := flag.Uint("recency", 12, "prune recency window (blocks) on top of the floor")
	paymentRequired := flag.Bool("payment-required", false, "require a paid channel for queries")
	operatorSeed := flag.String("operator-seed", "", "32-byte hex seed enabling signed attestations")
	adminToken := flag.String("admin-token", "", "bearer token for /admin/*")
	rate := flag.Int("rate", 0, "per-client requests/sec (0 = unlimited)")
	minDeposit := flag.Uint64("min-deposit", 0, "minimum channel deposit (abuse defense)")
	maxChannels := flag.Int("max-channels", 0, "max concurrent channels (0 = unlimited)")
	storeKind := flag.String("store", "mem", "store backend: mem | aerospike (aerospike needs -tags aerospike)")
	aeroHost := flag.String("aerospike-host", "127.0.0.1", "Aerospike host")
	aeroPort := flag.Int("aerospike-port", 3000, "Aerospike port")
	aeroNS := flag.String("aerospike-namespace", "memserve", "Aerospike namespace")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var src teranode.Source
	switch {
	case *mock:
		src = teranode.NewMock(teranode.MockConfig{Blocks: 1 << 30, SubtreesPer: 4, TxsPerSubtree: 2048, SpendFraction: 3})
	case *teranodeURL != "":
		src = httpsource.New(httpsource.Config{BaseURL: *teranodeURL, StartHeight: 0, PollEvery: time.Second})
	default:
		log.Error("provide -teranode <url> or -mock")
		os.Exit(2)
	}

	var seed []byte
	if *operatorSeed != "" {
		b, err := hex.DecodeString(*operatorSeed)
		if err != nil || len(b) != 32 {
			log.Error("operator-seed must be 32-byte hex")
			os.Exit(2)
		}
		seed = b
	}

	pol, err := prune.PolicyWithD(uint32(*reorg)+uint32(*recency), uint32(*reorg))
	if err != nil {
		log.Error("invalid prune policy", "err", err)
		os.Exit(2)
	}

	st, err := openStore(*storeKind, *aeroHost, *aeroPort, *aeroNS)
	if err != nil {
		log.Error("store init", "err", err)
		os.Exit(2)
	}

	srv, err := server.New(server.Config{
		ListenAddr:      *addr,
		ShardK:          *shardK,
		ShardID:         uint32(*shardID),
		Prune:           pol,
		Abuse:           paymentPolicy(*minDeposit, *maxChannels),
		PaymentRequired: *paymentRequired,
		OperatorSeed:    seed,
		AdminToken:      *adminToken,
		RatePerSec:      *rate,
		Store:           st,
	}, src, log)
	if err != nil {
		log.Error("server init", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Start(ctx); err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}

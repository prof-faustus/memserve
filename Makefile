# MemServe — build, test, run. BSV/Teranode only.

.PHONY: all build test race vet fmt fmtcheck demo ingest aerospike clean

all: fmtcheck vet build test

build:
	go build ./...

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmtcheck:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi

# Demonstrate + benchmark the shard server (ingest -> serve -> verify -> pay -> scale).
demo:
	go run ./cmd/memserve

# Run the ingest server against a mock Teranode source (shows spend-depth pruning).
ingest:
	go run ./cmd/ingest -blocks 200 -reorg 6 -recency 4

# Compile the Aerospike-backed store adapter (requires the client library).
aerospike:
	go get github.com/aerospike/aerospike-client-go/v7
	go build -tags aerospike ./...

clean:
	go clean ./...

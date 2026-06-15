# MemServe — build, test, run. BSV/Teranode only.

.PHONY: all build test race vet fmt fmtcheck demo ingest serve aerospike aerospike-up aerospike-test accelcheck cuda cuda-check clean

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

# Run the production daemon against the built-in mock source.
serve:
	go run ./cmd/memserved -mock -addr :8080

# Compile the Aerospike-backed store adapter (requires the client library).
aerospike:
	go get github.com/aerospike/aerospike-client-go/v7
	go build -tags aerospike ./...

# Bring up a local Aerospike for development / the conformance suite.
aerospike-up:
	docker compose -f deploy/docker-compose.yml up -d

# Run the store conformance suite against the local Aerospike cluster.
aerospike-test:
	go get github.com/aerospike/aerospike-client-go/v7
	AEROSPIKE_HOST=127.0.0.1 AEROSPIKE_PORT=3000 AEROSPIKE_NAMESPACE=memserve \
	  go test -tags aerospike ./store/aerospike/

# Validate the CPU verify backend against the Go reference + measure throughput.
accelcheck:
	go run ./cmd/accelcheck

# Build the CUDA GPU verify library and validate it against the reference (needs nvcc + GPU).
cuda:
	bash accel/cuda/build.sh

# Validate the CUDA backend (after `make cuda`); needs the lib on the linker/loader path.
cuda-check:
	CGO_LDFLAGS="-Laccel/cuda -lmemserve_gpu -lcudart" \
	LD_LIBRARY_PATH="accel/cuda:$$LD_LIBRARY_PATH" \
	  go run -tags cuda ./cmd/accelcheck

clean:
	go clean ./...

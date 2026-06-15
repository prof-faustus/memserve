# MemServe deployment

Infra-as-code for the three deployment fronts. These provision and validate; the build
itself is tested in CI.

## VM: Aerospike + memserved (fronts 1 & 2)

A single VM hosting the Aerospike store and the MemServe daemon.

```sh
# on a fresh Ubuntu VM (installs docker, clones, brings the stack up):
REPO_URL=https://github.com/prof-faustus/memserve.git bash deploy/vm/setup.sh

# or directly with compose:
cd deploy/vm && docker compose up -d --build
curl localhost:8080/readyz && curl localhost:8080/metrics
```

The stack runs immediately against the built-in **mock** source. To ingest a real chain
(front 1), edit `deploy/vm/docker-compose.yml`: drop in your **Teranode** asset server
(template provided) and change the daemon's `-mock` to `-teranode http://teranode:8090`.
Set `-operator-seed`, `-admin-token`, `-min-deposit`, `-rate` for production.

Validate the Aerospike store against the live cluster:

```sh
docker compose -f deploy/docker-compose.yml up -d   # standalone Aerospike
AEROSPIKE_HOST=127.0.0.1 AEROSPIKE_PORT=3000 AEROSPIKE_NAMESPACE=memserve \
  go test -tags aerospike ./store/aerospike/
```

## GPU box: multi-card verify backend (front 3)

The CUDA kernel splits each batch across **all visible NVIDIA cards** (e.g. 3). Build and
validate it on the GPU box:

```sh
REPO_DIR=$HOME/memserve bash deploy/gpu/setup.sh
# builds accel/cuda/libmemserve_gpu.so, then runs cmd/accelcheck:
#   PASS => the backend matches the Go reference and is safe to serve.
```

`accelcheck` is the correctness gate (`accel.Validate`): the GPU backend is trusted only
after it agrees bit-for-bit with the audited Go verifier.

## Remaining (needs the live systems)

- **BSV testnet**: broadcast the channel funding/commitment/refund/settlement txs to
  confirm consensus acceptance (DESIGN §10.6).
- **Teranode**: set the two endpoint templates to your node's real paths.
- **Aerospike**: run the conformance suite against your production cluster.
- **GPU**: run `deploy/gpu/setup.sh` on the multi-card box.

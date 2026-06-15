# MemServe daemon image. Default build is the pure in-memory store; pass TAGS=aerospike
# to build the Aerospike-backed daemon (the aerospike client is pure Go — CGO stays off).
#
#   docker build -t memserved .
#   docker build --build-arg TAGS=aerospike -t memserved:aerospike .
ARG TAGS=""

FROM golang:1.26 AS build
WORKDIR /src
COPY . .
ARG TAGS
RUN set -eux; \
    if [ -n "$TAGS" ]; then go get github.com/aerospike/aerospike-client-go/v7; fi; \
    CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -o /out/memserved ./cmd/memserved

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/memserved /memserved
EXPOSE 8080
ENTRYPOINT ["/memserved"]
CMD ["-mock", "-addr", ":8080"]

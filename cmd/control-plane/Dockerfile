FROM golang:1.25-bookworm AS builder

WORKDIR /app

# Copy module definition and download deps. replaycrypto is wired via a
# `replace ../../internal/replaycrypto` directive in go.mod, so it has to be
# staged before `go mod download` can resolve it.
COPY internal/replaycrypto/ ./internal/replaycrypto/
COPY cmd/control-plane/go.mod cmd/control-plane/go.sum ./cmd/control-plane/
RUN cd cmd/control-plane && go mod download

# Copy source (migrations live under cmd/control-plane/migrations/ and are go:embedded)
COPY cmd/control-plane/ ./cmd/control-plane/

# Build
WORKDIR /app/cmd/control-plane
RUN CGO_ENABLED=0 go build -o /control-plane .

# Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates git && rm -rf /var/lib/apt/lists/*
COPY --from=builder /control-plane /usr/local/bin/control-plane
ENTRYPOINT ["control-plane"]

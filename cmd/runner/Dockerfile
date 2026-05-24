FROM golang:1.25-bookworm AS builder

WORKDIR /app

# Copy module definition and download deps. The replaycrypto module is
# referenced via a `replace ../../internal/replaycrypto` directive in go.mod,
# so it has to be staged before `go mod download` can resolve it.
COPY internal/replaycrypto/ ./internal/replaycrypto/
COPY cmd/runner/go.mod cmd/runner/go.sum ./cmd/runner/
RUN cd cmd/runner && go mod download

# Copy source
COPY cmd/runner/ ./cmd/runner/

# Build the runner
WORKDIR /app/cmd/runner
RUN CGO_ENABLED=0 go build -o /runner .

# Runtime
FROM mcr.microsoft.com/playwright:v1.50.0-noble

# Install CA certificates and ffmpeg (used to extract keyframes from playwright videos for agent vision)
RUN apt-get update && apt-get install -y ca-certificates ffmpeg && rm -rf /var/lib/apt/lists/*

# Copy runner binary
COPY --from=builder /runner /usr/local/bin/runner

# Set up Node.js playwright project (used to run user scripts).
# Skip browser download — we use the browsers already in the playwright image.
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
WORKDIR /playwright-project
COPY cmd/runner/playwright-project/package.json cmd/runner/playwright-project/package-lock.json* ./
# Use `npm ci` when a lockfile is present (reproducible, the recommended path
# for CI/Docker builds). Falls back to `npm install` for repos that haven't
# committed a lockfile yet so the build still works.
RUN if [ -f package-lock.json ]; then npm ci --ignore-scripts; \
    else npm install --ignore-scripts; fi
COPY cmd/runner/playwright-project/playwright.config.ts ./
RUN mkdir -p tests

ENTRYPOINT ["runner"]

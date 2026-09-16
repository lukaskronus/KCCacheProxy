# ============================================
# Go-based KCCacheProxy Multi-arch Dockerfile
# ============================================

# Build stage - cross-compile for target platforms
FROM golang:1.22-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /src

# Copy go modules and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary for target platform (amd64 by default)
# Cross-compile for armv7/armv8 using buildx:
#   docker build --platform linux/arm/v7 -t kccp:armv7 .
#   docker build --platform linux/arm/v8 -t kccp:armv8 .
#   docker build --platform linux/amd64 -t kccp:amd64 .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o /app/main .

# ============================================
# Minimal final stage - scratch (5-15MB vs 120MB+)
# ============================================
FROM scratch

# Copy binary
COPY --from=builder /app/main /app/main

# Copy config.json
COPY docker/config.json /app/config.json

# Copy data directory template
COPY --from=builder /src/cache /app/cache 2>/dev/null || true

# Copy entrypoint (simple sh script that runs the binary)
COPY docker/docker-entrypoint.sh /
RUN chmod +x /docker-entrypoint.sh

# Minimal entrypoint - just run the binary
ENTRYPOINT [ "/app/main" ]

# Default command
CMD [ "proxy" ]

# ============================================
# Alternative: distroless for even better security
# ============================================
# FROM gcr.io/distroless/base-debian11 AS runtime
# COPY --from=builder /app/main /app/main
# ENTRYPOINT ["/app/main"]
# CMD ["proxy"]

# ============================================
# Multi-arch build instructions
# ============================================
# Using Docker Buildx:
#   docker buildx create --name mybuilder --use
#   docker buildx build \
#     --platform linux/arm/v7,linux/arm/v8,linux/amd64 \
#     -t kccp:latest \
#     --push .
#
# OR build individually:
#   docker build --platform linux/arm/v7 -t kccp:armv7 .
#   docker build --platform linux/arm/v8 -t kccp:armv8 .
#   docker build --platform linux/amd64 -t kccp:amd64 .
#
# ============================================
# Size comparison:
# - Node.js + Alpine:  ~120-150MB
# - Go + scratch:     ~5-15MB (87-96% smaller!)
# - Go + distroless:  ~25-40MB (72-83% smaller!)
# ============================================
# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download || true

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o insight-trace ./cmd/main.go

# Runtime stage
FROM alpine:3.18

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Copy binary from builder
COPY --from=builder /app/insight-trace /app/insight-trace

# Default environment variables
ENV SIDECAR_PORT=9090
ENV GRPC_PORT=9091
ENV METRICS_INTERVAL_SECONDS=5
ENV ANALYSIS_INTERVAL_SECONDS=10
ENV REPORT_INTERVAL_SECONDS=30
ENV MAX_METRICS_HISTORY=100

# Expose ports (HTTP REST + gRPC)
EXPOSE 9090 9091

# Run as non-root user
RUN adduser -D -u 1000 insight
USER insight

ENTRYPOINT ["/app/insight-trace"]

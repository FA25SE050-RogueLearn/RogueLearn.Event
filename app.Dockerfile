# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files first (better layer caching)
COPY go.mod go.sum ./

# Download dependencies separately for better caching
# This layer only rebuilds when go.mod or go.sum changes
RUN go mod download && go mod verify

# Copy source code
COPY . .

# Build with production optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o event ./cmd/main/main.go && \
    chmod +x /app/event

# Runtime stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/event .

# Expose port
EXPOSE 8083 8084

# Run the application
CMD [" /app/event "]

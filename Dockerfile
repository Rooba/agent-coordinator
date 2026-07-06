# Multi-stage build for Agent Coordinator

# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git make

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN make build

# Final stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1001 -S agent-coordinator && \
    adduser -u 1001 -S agent-coordinator -G agent-coordinator

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/build/agent-coordinator /usr/local/bin/agent-coordinator

# Copy example config
COPY --from=builder /app/config.example.json /app/config.json

# Set ownership
RUN chown -R agent-coordinator:agent-coordinator /app

# Switch to non-root user
USER agent-coordinator

# Expose port (if needed for future HTTP interface)
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD agent-coordinator version || exit 1

# Default command
CMD ["agent-coordinator"]
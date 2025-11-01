# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o function-vault-sync main.go

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

# Copy the binary from builder
COPY --from=builder /workspace/function-vault-sync /function-vault-sync

# Copy package metadata (required by Crossplane)
COPY package.yaml /package.yaml

# Use non-root user
USER 65532:65532

# Expose gRPC port
EXPOSE 9443

ENTRYPOINT ["/function-vault-sync"]

# Build stage — runs on the host architecture (ARM or x86).
# Go cross-compiles natively without QEMU: GOOS/GOARCH produce the correct binary.
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git file

WORKDIR /workspace

# Copy source. go.sum is generated during build via GONOSUMDB + -mod=mod.
COPY go.mod ./
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Cross-compile for linux/amd64.
# GONOSUMDB=* skips checksum DB (no go.sum needed upfront).
# -mod=mod lets Go resolve and download modules inline without go mod tidy.
RUN GONOSUMDB=* CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -mod=mod -a -ldflags="-s -w" -o manager ./cmd/main.go

# Verify the binary is actually amd64.
RUN file manager

# Final image — explicitly amd64 platform.
FROM scratch

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

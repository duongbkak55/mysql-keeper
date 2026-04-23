# Build stage. BUILDPLATFORM is the runner's architecture; TARGETPLATFORM /
# TARGETOS / TARGETARCH are the architecture we're cross-compiling FOR and
# are injected automatically by `docker buildx build --platform=...`.
# Running go on BUILDPLATFORM + setting GOOS/GOARCH from TARGETARCH avoids
# the QEMU-in-Go-compiler slow path for every non-native arch.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git file

WORKDIR /workspace

# Copy source. go.sum is generated during build via GONOSUMDB + -mod=mod.
COPY go.mod ./
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Cross-compile for the TARGETARCH the buildx platform requested. For
# non-buildx (plain `docker build`) TARGETOS/TARGETARCH are unset and the
# shell defaults substitute the runner's arch, giving the old behaviour.
RUN GONOSUMDB=* CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -mod=mod -a -ldflags="-s -w" -o manager ./cmd/main.go

# Verify the binary is actually the architecture we asked for.
RUN file manager

# Final image.
FROM scratch

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

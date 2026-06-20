# syntax=docker/dockerfile:1

# --- build stage ---------------------------------------------------------
# Build on the native platform and cross-compile to the target arch so multi-
# arch builds (linux/amd64 + linux/arm64) stay fast.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/webhook ./cmd/webhook

# --- runtime stage -------------------------------------------------------
# distroless static: no shell, no package manager, runs as a non-root user
# (uid 65532). The binary is fully static (CGO disabled).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/webhook /webhook

# 8888: ExternalDNS provider API (localhost). 8080: health + metrics.
EXPOSE 8888 8080

USER nonroot:nonroot
ENTRYPOINT ["/webhook"]

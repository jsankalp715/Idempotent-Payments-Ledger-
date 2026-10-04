# syntax=docker/dockerfile:1.7

# ---- build ----------------------------------------------------------------
# go.mod declares the minimum Go version (1.26); release images are built
# with the latest stable toolchain.
FROM golang:1.27-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

# Behind a TLS-intercepting proxy, pass its CA as a build secret:
#   docker build --secret id=extra_ca,src=/path/to/ca.pem .
# Without the secret only the system trust store is used.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then \
        cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; \
        export SSL_CERT_FILE=/tmp/ca.pem; \
    fi; \
    go mod download && go mod verify

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/ledger ./cmd/ledger

# ---- runtime --------------------------------------------------------------
# Static binary on distroless: no shell, no package manager, non-root user,
# CA certificates included (for sslmode=verify-full to managed Postgres).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledger /ledger
USER nonroot:nonroot
ENV HTTP_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=5 CMD ["/ledger", "healthcheck"]
ENTRYPOINT ["/ledger"]
CMD ["serve"]

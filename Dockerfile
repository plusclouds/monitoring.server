# Static binary on distroless (ADR-0011). Build with:
#   docker build --build-arg VERSION=$(git describe --tags --always) \
#                --build-arg COMMIT=$(git rev-parse HEAD) -t monitor .
# Or run the whole stack with: docker compose -f deploy/docker-compose.yml up -d
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/plusclouds/monitoring.server/internal/buildinfo.Version=${VERSION} -X github.com/plusclouds/monitoring.server/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/monitor ./cmd/monitor

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="monitoring.server" \
      org.opencontainers.image.description="API-first monitoring engine" \
      org.opencontainers.image.source="https://github.com/plusclouds/monitoring.server" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/monitor /usr/local/bin/monitor
USER nonroot:nonroot
# REST API. The status server (9090) listens on loopback inside the
# container unless the config binds it elsewhere with TLS and a token.
EXPOSE 8443
# No shell in the image: the binary probes its own readiness endpoint.
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/monitor", "health"]
ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["serve", "--config", "/etc/monitor/config.yaml"]

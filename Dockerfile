# Static binary on distroless (ADR-0011). Build with:
#   docker build --build-arg VERSION=$(git describe --tags --always) \
#                --build-arg COMMIT=$(git rev-parse HEAD) -t monitor .
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
COPY --from=build /out/monitor /usr/local/bin/monitor
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/monitor"]
CMD ["serve", "--config", "/etc/monitor/config.yaml"]

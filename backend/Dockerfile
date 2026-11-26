FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS toolchain

FROM toolchain AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/ ./cmd/api ./cmd/worker ./cmd/migrate

FROM toolchain AS river-build
ARG GOPROXY=https://proxy.golang.org,direct
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 GOBIN=/river-bin go install github.com/riverqueue/river/cmd/river@v0.51.2

FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS runtime
WORKDIR /app
COPY --from=build /out/ /app/
RUN mkdir -p /var/lib/autolife/restore && chown 65534:65534 /var/lib/autolife/restore && chmod 0700 /var/lib/autolife/restore
USER 65534:65534

FROM runtime AS migrations
COPY --from=river-build /river-bin/river /app/river

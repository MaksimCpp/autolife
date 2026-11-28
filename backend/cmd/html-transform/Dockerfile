FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/htmlproto ./internal/htmlproto
COPY internal/pdfcheck ./internal/pdfcheck
COPY cmd/html-transform ./cmd/html-transform
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /autolife-html ./cmd/html-transform
FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e
RUN apt-get update && apt-get install -y --no-install-recommends chromium qpdf fonts-liberation fonts-noto-core && rm -rf /var/lib/apt/lists/*
RUN rmdir /tmp && ln -s /work /tmp
COPY --from=build /autolife-html /usr/local/bin/autolife-html
ENTRYPOINT ["/usr/local/bin/autolife-html"]

FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/tableproto ./internal/tableproto
COPY cmd/table-transform ./cmd/table-transform
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /autolife-table ./cmd/table-transform
FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
COPY --from=build /autolife-table /usr/local/bin/autolife-table
ENTRYPOINT ["/usr/local/bin/autolife-table"]

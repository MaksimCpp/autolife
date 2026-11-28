FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/file-inspector ./cmd/file-inspector
COPY internal/htmlproto ./internal/htmlproto
COPY internal/tableproto ./internal/tableproto
COPY internal/svgcheck ./internal/svgcheck
COPY internal/pdfcheck ./internal/pdfcheck
COPY internal/officeproto ./internal/officeproto
COPY internal/pdfproto ./internal/pdfproto
COPY internal/sandbox/launcher.go internal/sandbox/transform.go internal/sandbox/usage.go ./internal/sandbox/
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /inspector ./cmd/file-inspector
FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS image
RUN apk add --no-cache vips-tools font-liberation
COPY --from=build /inspector /usr/local/bin/inspector
RUN printf '#!/bin/sh\nexec /usr/local/bin/inspector image\n' > /usr/local/bin/autolife-inspect && chmod 0555 /usr/local/bin/autolife-inspect
FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS pdf
RUN apk add --no-cache qpdf
COPY --from=build /inspector /usr/local/bin/inspector
RUN printf '#!/bin/sh\nexec /usr/local/bin/inspector pdf\n' > /usr/local/bin/autolife-inspect && chmod 0555 /usr/local/bin/autolife-inspect

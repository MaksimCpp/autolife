FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS wrapper
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/svgcheck ./internal/svgcheck
COPY cmd/svg-image ./cmd/svg-image
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /autolife-image ./cmd/svg-image

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS base
ARG VIPS_VERSION=8.17.3-r1
RUN apk add --no-cache "vips=$VIPS_VERSION" font-liberation

FROM base AS build
RUN apk add --no-cache build-base pkgconf "vips-dev=$VIPS_VERSION"
COPY cmd/image-transform/main.c /src/main.c
RUN cc -std=c17 -O2 -Wall -Wextra -Werror -o /autolife-image /src/main.c $(pkg-config --cflags --libs vips) -lm

FROM base
COPY --from=build /autolife-image /usr/local/bin/image-native
COPY --from=wrapper /autolife-image /usr/local/bin/autolife-image
ENTRYPOINT ["/usr/local/bin/autolife-image"]

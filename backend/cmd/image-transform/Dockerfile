FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS base
ARG VIPS_VERSION=8.17.3-r1
RUN apk add --no-cache "vips=$VIPS_VERSION"

FROM base AS build
RUN apk add --no-cache build-base pkgconf "vips-dev=$VIPS_VERSION"
COPY main.c /src/main.c
RUN cc -std=c17 -O2 -Wall -Wextra -Werror -o /autolife-image /src/main.c $(pkg-config --cflags --libs vips) -lm

FROM base
COPY --from=build /autolife-image /usr/local/bin/autolife-image
ENTRYPOINT ["/usr/local/bin/autolife-image"]

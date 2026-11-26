FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 GO111MODULE=off go build -trimpath -o /probe main.go && mkdir -m 0777 /probe-writable

FROM scratch
ARG PROBE_PROFILE
LABEL io.autolife.sandbox.probe=$PROBE_PROFILE
COPY --from=build /probe /usr/local/bin/autolife-inspect
COPY --from=build /probe-writable /probe-writable

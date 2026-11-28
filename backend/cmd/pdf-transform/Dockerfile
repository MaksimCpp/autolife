FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS go-build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/pdf-transform ./cmd/pdf-transform
COPY internal/pdfcheck ./internal/pdfcheck
COPY internal/pdfproto ./internal/pdfproto
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /autolife-pdf ./cmd/pdf-transform

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS image-build
RUN apk add --no-cache build-base pkgconf vips-dev=8.17.3-r1
COPY cmd/image-transform/main.c /src/main.c
RUN cc -std=c17 -O2 -Wall -Wextra -Werror -o /autolife-image /src/main.c $(pkg-config --cflags --libs vips) -lm

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS python-build
RUN apk add --no-cache py3-pip
COPY cmd/pdf-transform/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --require-hashes --only-binary=:all: --no-deps --target /opt/img2pdf -r /tmp/requirements.txt

FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2
RUN apk add --no-cache qpdf=12.2.0-r0 vips=8.17.3-r1 python3 py3-pillow
COPY --from=go-build /autolife-pdf /usr/local/bin/autolife-pdf
COPY --from=image-build /autolife-image /usr/local/bin/autolife-image
COPY --from=python-build /opt/img2pdf /opt/img2pdf
COPY cmd/pdf-transform/rasterize.py /usr/local/lib/autolife/rasterize.py
ENV PYTHONPATH=/opt/img2pdf PYTHONDONTWRITEBYTECODE=1
ENTRYPOINT ["/usr/local/bin/autolife-pdf"]

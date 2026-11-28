FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/office-transform/main.go ./cmd/office-transform/main.go
COPY internal/pdfcheck ./internal/pdfcheck
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /check-office-pdf ./cmd/office-transform

FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends libreoffice-writer libreoffice-calc libreoffice-impress python3-uno python3-olefile python3-defusedxml qpdf fonts-liberation fonts-noto-core && rm -rf /var/lib/apt/lists/*
COPY --from=build /check-office-pdf /usr/local/bin/check-office-pdf
FROM runtime AS smoke
RUN apt-get update && apt-get install -y --no-install-recommends poppler-utils && rm -rf /var/lib/apt/lists/*
COPY --chmod=0555 cmd/office-transform/office.py /usr/local/bin/autolife-office
COPY cmd/office-transform/test_office.py /usr/local/share/test-office.py
ENTRYPOINT ["/usr/bin/python3", "/usr/local/share/test-office.py"]
RUN rmdir /tmp && ln -s /work /tmp
FROM runtime AS production
RUN rmdir /tmp && ln -s /work /tmp
COPY --chmod=0555 cmd/office-transform/office.py /usr/local/bin/autolife-office
ENTRYPOINT ["/usr/local/bin/autolife-office"]

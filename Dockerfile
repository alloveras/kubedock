FROM docker.io/golang:1.26-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=latest
ARG BUILD=unknown
ARG DATE=unknown
ARG IMAGE=ghcr.io/alloveras/kubedock:latest
RUN CGO_ENABLED=0 go build \
    -tags exclude_graphdriver_btrfs \
    -ldflags "-s -w \
      -X github.com/joyrex2001/kubedock/internal/config.Version=${VERSION} \
      -X github.com/joyrex2001/kubedock/internal/config.Build=${BUILD} \
      -X github.com/joyrex2001/kubedock/internal/config.Date=${DATE} \
      -X github.com/joyrex2001/kubedock/internal/config.Image=${IMAGE}" \
    -o /kubedock .

FROM docker.io/alpine:latest@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11 AS certs
RUN apk --update add ca-certificates

FROM docker.io/busybox:stable-musl@sha256:3c6ae8008e2c2eedd141725c30b20d9c36b026eb796688f88205845ef17aa213
COPY --from=builder /kubedock /usr/local/bin/kubedock
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/usr/local/bin/kubedock"]
CMD ["server"]

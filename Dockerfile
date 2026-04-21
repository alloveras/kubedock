FROM docker.io/golang:1.25-alpine AS builder
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

FROM docker.io/alpine:latest AS certs
RUN apk --update add ca-certificates

FROM docker.io/busybox:latest
COPY --from=builder /kubedock /usr/local/bin/kubedock
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/usr/local/bin/kubedock"]
CMD ["server"]

# syntax=docker/dockerfile:1

# Pinned to the toolchain declared in go.mod so builder and language version
# never drift apart. Bump both together.
FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependencies first so the module layer caches across source edits.
#
# GOPROXY is a build arg because proxy.golang.org resolves to
# storage.googleapis.com, which is unreliable from mainland China. Override it
# on the build command line if you build elsewhere:
#   docker build --build-arg GOPROXY=https://proxy.golang.org,direct .
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/naoki66/ac-controller/internal/version.Version=${VERSION} \
        -X github.com/naoki66/ac-controller/internal/version.Commit=${COMMIT} \
        -X github.com/naoki66/ac-controller/internal/version.Date=${DATE}" \
      -o /out/ac ./cmd/ac

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 1000 ac \
 && mkdir -p /var/lib/ac \
 && chown ac:ac /var/lib/ac

COPY --from=build /out/ac /usr/local/bin/ac

USER ac
ENV AC_DATA_DIR=/var/lib/ac \
    AC_HTTP_ADDR=:8080 \
    AC_DISCOVER_ADDR=:29820

VOLUME ["/var/lib/ac"]
EXPOSE 8080/tcp
EXPOSE 29820/udp

ENTRYPOINT ["/usr/local/bin/ac"]
CMD ["serve"]

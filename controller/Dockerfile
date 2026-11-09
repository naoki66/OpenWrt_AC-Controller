# syntax=docker/dockerfile:1

# Pinned to the toolchain declared in go.mod so builder and language version
# never drift apart. Bump both together.
# --platform=$BUILDPLATFORM 让构建阶段始终跑在宿主机架构上，
# 配合下面的 TARGETOS/TARGETARCH 交叉编译目标架构的二进制，
# buildx 构建多架构镜像时不需要 QEMU 模拟整个 Go 编译过程。
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
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
# buildx 自动注入目标平台；普通 docker build 下为空，go 视为未设置、用本机默认值。
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-} GOARCH=${TARGETARCH:-} go build -trimpath \
      -ldflags "-s -w \
        -X github.com/naoki66/ac-controller/shared/version.Version=${VERSION} \
        -X github.com/naoki66/ac-controller/shared/version.Commit=${COMMIT} \
        -X github.com/naoki66/ac-controller/shared/version.Date=${DATE}" \
      -o /out/ac ./controller/cmd/ac

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 1000 ac \
 && mkdir -p /var/lib/ac \
 && chown ac:ac /var/lib/ac

COPY --from=build /out/ac /usr/local/bin/ac

USER ac
ENV AC_DATA_DIR=/var/lib/ac \
    AC_HTTP_ADDR=:8090 \
    AC_DISCOVER_ADDR=:29820

VOLUME ["/var/lib/ac"]
EXPOSE 8090/tcp
EXPOSE 29820/udp

ENTRYPOINT ["/usr/local/bin/ac"]
CMD ["serve"]

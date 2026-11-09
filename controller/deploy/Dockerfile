# 离线运行镜像，不依赖任何基础层，仅 `docker compose up -d --build` 时使用。
#
# ac 是 CGO_ENABLED=0 的静态二进制（SQLite 用纯 Go 驱动），所以 scratch 就够。
# 目标宿主没有外网，这样连 FROM 都不用拉取，构建全程离线。
#
# 代价：容器里没有 shell、没有 wget，因此没有 healthcheck。看状态用
#   docker logs ac-controller
#   curl http://<AC_IP>:8090/api/v1/health
FROM scratch

# --chmod 是必需的：二进制在 Windows 上编译，落到宿主机时是 644，
# 而 scp 不会补可执行位，直接 COPY 会导致 "exec: /ac: permission denied"。
COPY --chmod=0755 ac /ac

# 与数据目录的属主一致，见部署脚本里的 chown 1000:1000。
USER 1000:1000

ENV AC_DATA_DIR=/var/lib/ac \
    AC_HTTP_ADDR=:8090 \
    AC_DISCOVER_ADDR=:29820

EXPOSE 8090/tcp
EXPOSE 29820/udp

ENTRYPOINT ["/ac"]
CMD ["serve"]

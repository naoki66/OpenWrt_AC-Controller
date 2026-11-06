# E2E 镜像：把 agent 与假的 ubus/wifi/init.d 一起烤进镜像，避免 Windows 挂载丢失可执行位。
FROM alpine:3.21
COPY ac-agent /usr/local/bin/ac-agent
COPY bin/ubus /usr/local/bin/ubus
COPY bin/wifi /usr/local/bin/wifi
COPY bin/iw /usr/local/bin/iw
COPY bin/ip /usr/local/bin/ip
RUN chmod +x /usr/local/bin/ac-agent /usr/local/bin/ubus /usr/local/bin/wifi \
             /usr/local/bin/iw /usr/local/bin/ip

# 假的 dnsmasq/odhcpd 服务脚本：记录调用，供断言"AP 模式已停 DHCP/DNS"。
RUN mkdir -p /etc/init.d \
 && for s in dnsmasq odhcpd nrsyncd usteer dawn; do \
      printf '#!/bin/sh\necho "%s $*" >> /root/calls.log\nexit 0\n' "$s" > /etc/init.d/$s; \
      chmod +x /etc/init.d/$s; \
    done

ENV AC_ROOT=/root
ENTRYPOINT ["/usr/local/bin/ac-agent"]
CMD ["status"]

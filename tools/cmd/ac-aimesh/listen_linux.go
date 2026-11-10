//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func listen(name string, duration time.Duration, visit func(time.Time, []byte) error) error {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	// 只监听指定接口，不开启混杂模式、不发送探测报文，也不修改网络配置。
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("packet socket (requires root/CAP_NET_RAW): %w", err)
	}
	defer unix.Close(fd)
	// sockaddr_ll 的协议字段使用网络字节序，需按主机字节序构造对应数值。
	protocol := binary.NativeEndian.Uint16([]byte{0, unix.ETH_P_ALL})
	if err = unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: protocol}); err != nil {
		return err
	}
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		return err
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	deadline := time.Now().Add(duration)
	buffer := make([]byte, 65536)
	for time.Now().Before(deadline) {
		select {
		case <-interrupt:
			return nil
		default:
		}
		n, _, err := unix.Recvfrom(fd, buffer, unix.MSG_TRUNC)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			continue
		}
		if err != nil {
			return err
		}
		if n > len(buffer) {
			continue
		}
		if err := visit(time.Now().UTC(), buffer[:n]); err != nil {
			return err
		}
	}
	return nil
}

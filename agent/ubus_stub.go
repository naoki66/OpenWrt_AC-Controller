//go:build !linux

package agent

import "context"

// ubusd listens on a unix domain socket, so the transport only exists on Linux.
// The agent ships for OpenWrt only; this stub exists so `go test ./...` and the
// developer build keep working, and it reports the same sentinel a Linux board
// without the socket would, which keeps the fallback paths identical.

type unavailableTransport struct{}

func newUbusTransport() ubusTransport { return unavailableTransport{} }

func (unavailableTransport) Lookup(context.Context, string) (uint32, error) {
	return 0, ErrUbusUnavailable
}

func (unavailableTransport) Invoke(context.Context, uint32, string) (map[string]any, error) {
	return nil, ErrUbusUnavailable
}

func (unavailableTransport) Close() error { return nil }

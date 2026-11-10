//go:build !linux

package main

import (
	"errors"
	"time"
)

func listen(_ string, _ time.Duration, _ func(time.Time, []byte) error) error {
	return errors.New("live capture requires Linux; use -pcap for offline inspection")
}

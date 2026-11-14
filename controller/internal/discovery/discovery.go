package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

type Listener struct {
	Addr        string
	ACID        string
	HTTPPort    int
	AdvertiseIP string
	SigningKey  string
	// SecretRequired mirrors whether the controller actually enforces
	// AC_SHARED_SECRET. Advertising true when no secret is configured would make
	// the AP look for credentials that cannot exist. Static default; a runtime
	// rotation via the management API is tracked by SecretRequiredFunc instead.
	SecretRequired bool
	// SecretRequiredFunc, when set, wins over SecretRequired so the offer
	// tracks the live effective secret (it is consulted per offer).
	SecretRequiredFunc func() bool
	Log                *slog.Logger
}

// secretRequired reports whether APs must authenticate right now.
func (l *Listener) secretRequired() bool {
	if l.SecretRequiredFunc != nil {
		return l.SecretRequiredFunc()
	}
	return l.SecretRequired
}

func (l *Listener) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp4", l.Addr)
	if err != nil {
		return err
	}
	uc, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	defer uc.Close()

	l.Log.Info("discovery listening", "addr", uc.LocalAddr().String())
	return l.serve(ctx, uc)
}

// serve runs the read loop on an already bound socket. Split out so tests can
// bind an ephemeral port and still exercise the real packet handling.
func (l *Listener) serve(ctx context.Context, uc *net.UDPConn) error {
	l.Log.Info("discovery loop started", "addr", uc.LocalAddr().String())

	buf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Deadline keeps the loop responsive to cancellation.
		_ = uc.SetReadDeadline(time.Now().Add(time.Second))
		n, peer, err := uc.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			l.Log.Warn("discovery read failed", "err", err)
			continue
		}
		l.handle(uc, buf[:n], peer)
	}
}

func (l *Listener) handle(uc *net.UDPConn, payload []byte, peer *net.UDPAddr) {
	var p proto.Discover
	if err := json.Unmarshal(payload, &p); err != nil {
		return // not ours, stay quiet
	}
	mac := NormalizeMAC(p.MAC)
	switch p.Type {
	case proto.MsgDiscover:
		if mac == "" {
			return
		}
	case proto.MsgLookup:
		if p.ACID == "" || p.ACID != l.ACID {
			return
		}
	default:
		return
	}

	// UDP only supplies the enrollment address. Device state and metadata
	// change only after authentication on the AP-facing HTTP endpoints.
	reply := proto.Offer{
		Type:           proto.MsgOffer,
		V:              proto.ProtocolVersion,
		ACID:           l.ACID,
		ACIP:           l.advertiseIP(peer),
		HTTPPort:       l.HTTPPort,
		SecretRequired: l.secretRequired(),
		SigningKey:     l.SigningKey,
	}
	out, err := json.Marshal(reply)
	if err != nil {
		return
	}
	if _, err := uc.WriteToUDP(out, peer); err != nil {
		l.Log.Warn("offer failed", "peer", peer.String(), "err", err)
		return
	}
	l.Log.Debug("ap discovery offer sent", "mac", mac, "ip", peer.IP.String())
}

// advertiseIP reports the local address the AP should connect to. Deriving it
// from the routing table beats a configured value on multi-homed hosts.
func (l *Listener) advertiseIP(peer *net.UDPAddr) string {
	if l.AdvertiseIP != "" {
		return l.AdvertiseIP
	}
	c, err := net.DialUDP("udp4", nil, peer)
	if err != nil {
		return ""
	}
	defer c.Close()
	if la, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return la.IP.String()
	}
	return ""
}

// NormalizeMAC canonicalises a MAC address to lower-case colon form. It returns
// an empty string for anything that is not six hex octets.
func NormalizeMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", ":")
	if len(s) != 17 {
		return ""
	}
	for i, r := range s {
		if i%3 == 2 {
			if r != ':' {
				return ""
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return ""
		}
	}
	return s
}

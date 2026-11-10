package agent

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/shared/proto"
)

// The heartbeat budget is what keeps an offline AP on schedule. A single total
// timeout hides the difference between "the controller is slow" and "the
// controller is gone", and the agent's cycle is serial: everything else waits
// behind that call.
func TestControllerClientHasSplitTimeouts(t *testing.T) {
	c := NewClient("http://127.0.0.1:8090", "")

	if c.HTTP.Timeout != controllerTotalTimeout {
		t.Errorf("total timeout = %s, want %s", c.HTTP.Timeout, controllerTotalTimeout)
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", c.HTTP.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("no dial timeout: a black-holed controller would hang for the whole budget")
	}
	if tr.ResponseHeaderTimeout != controllerHeaderTimeout {
		t.Errorf("header timeout = %s, want %s", tr.ResponseHeaderTimeout, controllerHeaderTimeout)
	}
	if tr.DisableKeepAlives {
		t.Error("keep-alive disabled: every heartbeat would pay a new handshake")
	}
}

// 控制器拒绝连接时应及时失败，因此使用已关闭的本地端口。
// TEST-NET 地址可能受宿主机路由或代理影响，无法稳定模拟不可达状态。
func TestHelloFailsInDialTimeWhenControllerIsUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c := NewClient("http://"+addr, "")
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.Proxy = nil // an environment proxy would answer instead of the void
	}

	start := time.Now()
	_, err = c.Hello(context.Background(), proto.Hello{MAC: "00:00:00:00:00:21"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("hello to an unroutable address must fail")
	}
	// Dial timeout plus a generous margin for scheduling on a loaded host.
	if limit := controllerDialTimeout + 2*time.Second; elapsed > limit {
		t.Fatalf("hello took %s, want <= %s", elapsed, limit)
	}
}

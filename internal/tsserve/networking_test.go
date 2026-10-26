package tsserve_test

import (
	"context"
	"testing"

	"github.com/scottjab/folio/internal/tsserve"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
)

func TestStartPreservesSOCKSProxy(t *testing.T) {
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("TS_CONTROL_URL", "http://127.0.0.1:1")
	t.Setenv("TS_NO_LOGS_NO_SUPPORT", "1")
	netns.SetEnabled(true)
	t.Cleanup(func() { netns.SetEnabled(true) })
	monitor := netmon.NewStatic()
	proxyEnabled := func() bool {
		return netns.IsSOCKSDialer(netns.NewDialer(t.Logf, monitor))
	}
	if !proxyEnabled() {
		t.Fatal("SOCKS proxy must be enabled before starting folio")
	}

	// Cancellation exercises startup without depending on a control server.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	srv, err := tsserve.Start(ctx, tsserve.Options{
		Hostname: "proxy-test",
		StateDir: t.TempDir(),
		Log:      quietLogger(),
	})
	if err == nil {
		srv.Close()
		t.Fatal("startup with a canceled context succeeded")
	}
	if !proxyEnabled() {
		t.Fatal("folio startup disabled ALL_PROXY for control and DERP traffic")
	}
}

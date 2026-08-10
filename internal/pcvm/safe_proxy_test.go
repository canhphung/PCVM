package pcvm

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSafeProxyDialRejectsDNSRebindingBeforeDial(t *testing.T) {
	var dialed atomic.Int32
	dial := newSafeProxyDialContext("public.example", "443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return nil, nil
		},
	)
	if _, err := dial(context.Background(), "tcp", "public.example:443"); err == nil || !strings.Contains(err.Error(), "blocked address") {
		t.Fatalf("rebinding dial error=%v", err)
	}
	if dialed.Load() != 0 {
		t.Fatalf("network dial occurred %d times after blocked DNS answer", dialed.Load())
	}
}

func TestSafeProxyDialUsesValidatedIPDirectly(t *testing.T) {
	var address string
	peerClosed := make(chan struct{})
	dial := newSafeProxyDialContext("public.example", "8443",
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		},
		func(_ context.Context, network, target string) (net.Conn, error) {
			if network != "tcp" {
				t.Fatalf("network=%q", network)
			}
			address = target
			client, peer := net.Pipe()
			go func() {
				_ = peer.Close()
				close(peerClosed)
			}()
			return client, nil
		},
	)
	connection, err := dial(context.Background(), "tcp", "public.example:8443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	<-peerClosed
	if address != "1.1.1.1:8443" {
		t.Fatalf("dial target=%q; want validated IP", address)
	}
	if _, err := dial(context.Background(), "tcp", "other.example:8443"); err == nil {
		t.Fatal("unexpected transport target accepted")
	}
}

func TestSafeProxyBindsLoopbackAndCleanupIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lookup := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}
	target, cleanup, err := startSafeProxyWith(ctx, "https://public.example", lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(target, "http://127.0.0.1:") {
		t.Fatalf("safe proxy target=%q", target)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	cancel()
}

func TestPublicProxyIPRejectsSpecialPurposeRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1", "100.64.0.1", "192.0.2.1", "192.88.99.1", "198.18.0.1",
		"64:ff9b::7f00:1", "64:ff9b:1::a00:1", "100::1", "2001:2::1", "2001:db8::1", "2002:7f00:1::",
	} {
		if publicProxyIP(net.ParseIP(raw)) {
			t.Errorf("special-purpose address accepted: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicProxyIP(net.ParseIP(raw)) {
			t.Errorf("public address rejected: %s", raw)
		}
	}
}

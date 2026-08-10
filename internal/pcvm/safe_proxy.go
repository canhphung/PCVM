package pcvm

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var blockedProxyPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func validateWebProxy(mode, upstream string) error {
	return validateWebProxyWith(mode, upstream, DefaultDependencies().LookupIP)
}

func validateWebProxyWith(mode, upstream string, lookup DNSLookupFunc) error {
	_, err := canonicalWebProxyWith(mode, upstream, lookup)
	return err
}

// canonicalWebProxy validates the initial DNS view for fast feedback. The
// embedded safe proxy repeats the same validation for every new upstream TCP
// connection and dials the approved IP directly, so the public web server
// never performs a second, attacker-controlled DNS lookup.
func canonicalWebProxy(mode, upstream string) (string, error) {
	return canonicalWebProxyWith(mode, upstream, DefaultDependencies().LookupIP)
}

func canonicalWebProxyWith(mode, upstream string, lookup DNSLookupFunc) (string, error) {
	if lookup == nil {
		lookup = DefaultDependencies().LookupIP
	}
	if mode != "static" && mode != "proxy" {
		return "", fmt.Errorf("WEB_MODE must be static or proxy")
	}
	if mode == "static" {
		return "", nil
	}
	if strings.ContainsAny(upstream, " \t\r\n;{}#\"'\\$`") {
		return "", fmt.Errorf("UPSTREAM_URL contains characters that are unsafe in managed server configuration")
	}
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.Opaque != "" || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("UPSTREAM_URL must be an HTTP(S) URL without credentials or fragments")
	}
	host := strings.ToLower(parsed.Hostname())
	if ip := net.ParseIP(host); ip == nil {
		if len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
			return "", fmt.Errorf("UPSTREAM_URL has an invalid hostname")
		}
		for _, r := range host {
			if r > unicode.MaxASCII || !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-' {
				return "", fmt.Errorf("UPSTREAM_URL hostname must use ASCII DNS labels")
			}
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return "", fmt.Errorf("UPSTREAM_URL has an invalid DNS label")
			}
		}
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("UPSTREAM_URL has an invalid port")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := resolvePublicProxyIPs(ctx, host, lookup); err != nil {
		return "", err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	canonical := parsed.String()
	if strings.ContainsAny(canonical, " \t\r\n;{}#\"'\\$`") {
		return "", fmt.Errorf("UPSTREAM_URL cannot be represented safely in managed server configuration")
	}
	return canonical, nil
}

func resolvePublicProxyIPs(ctx context.Context, host string, lookup func(context.Context, string) ([]net.IPAddr, error)) ([]net.IP, error) {
	if literal := net.ParseIP(host); literal != nil {
		if !publicProxyIP(literal) {
			return nil, fmt.Errorf("UPSTREAM_URL host %q resolves to blocked address %s", host, literal.String())
		}
		return []net.IP{literal}, nil
	}
	addresses, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve UPSTREAM_URL host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve UPSTREAM_URL host %q: no addresses", host)
	}
	approved := make([]net.IP, 0, len(addresses))
	seen := map[string]bool{}
	for _, address := range addresses {
		if !publicProxyIP(address.IP) {
			return nil, fmt.Errorf("UPSTREAM_URL host %q resolves to blocked address %s", host, address.IP.String())
		}
		key := address.IP.String()
		if !seen[key] {
			seen[key] = true
			approved = append(approved, append(net.IP(nil), address.IP...))
		}
	}
	return approved, nil
}

func publicProxyIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedProxyPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

type safeProxy struct {
	server    *http.Server
	listener  net.Listener
	transport *http.Transport
	once      sync.Once
	closeErr  error
	done      chan struct{}
}

func startSafeProxy(parent context.Context, canonical string) (string, func() error, error) {
	return startSafeProxyWith(parent, canonical, DefaultDependencies().LookupIP)
}

func startSafeProxyWith(parent context.Context, canonical string, lookup DNSLookupFunc) (string, func() error, error) {
	target, err := url.Parse(canonical)
	if err != nil || target.Scheme != "http" && target.Scheme != "https" || target.Hostname() == "" {
		return "", nil, fmt.Errorf("invalid canonical safe-proxy target")
	}
	if lookup == nil {
		lookup = DefaultDependencies().LookupIP
	}
	host, port := target.Hostname(), target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       64,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
	}
	transport.DialContext = newSafeProxyDialContext(host, port, lookup, dialer.DialContext)
	reverse := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.SetURL(target)
			request.Out.Host = target.Host
		},
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "PCVM upstream unavailable", http.StatusBadGateway)
		},
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		transport.CloseIdleConnections()
		return "", nil, fmt.Errorf("start safe proxy listener: %w", err)
	}
	server := &http.Server{
		Handler:           reverse,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       75 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	proxy := &safeProxy{server: server, listener: listener, transport: transport, done: make(chan struct{})}
	go func() { _ = server.Serve(listener) }()
	go func() {
		select {
		case <-parent.Done():
			_ = proxy.close()
		case <-proxy.done:
		}
	}()
	closeProxy := proxy.close
	return "http://" + listener.Addr().String(), closeProxy, nil
}

type proxyDialFunc func(context.Context, string, string) (net.Conn, error)

func newSafeProxyDialContext(host, port string, lookup func(context.Context, string) ([]net.IPAddr, error), dial proxyDialFunc) proxyDialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("safe proxy refuses network %q", network)
		}
		addressHost, addressPort, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(strings.TrimSuffix(addressHost, "."), strings.TrimSuffix(host, ".")) || addressPort != port {
			return nil, fmt.Errorf("safe proxy refuses unexpected target %q", address)
		}
		addresses, err := resolvePublicProxyIPs(ctx, host, lookup)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range addresses {
			connection, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			last = dialErr
		}
		return nil, fmt.Errorf("connect to validated UPSTREAM_URL host %q: %w", host, last)
	}
}

func (p *safeProxy) close() error {
	p.once.Do(func() {
		close(p.done)
		p.transport.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.closeErr = p.server.Shutdown(ctx)
		if p.closeErr != nil {
			_ = p.server.Close()
		}
	})
	return p.closeErr
}

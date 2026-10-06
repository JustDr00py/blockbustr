package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func ask(t *testing.T, server net.Addr, msg string) (map[string]any, bool) {
	t.Helper()
	c, err := net.DialUDP("udp", nil, server.(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(buf[:n], &m); err != nil {
		t.Fatalf("reply %q: %v", buf[:n], err)
	}
	return m, true
}

func serve(t *testing.T, info Info) net.Addr {
	t.Helper()
	s, err := Listen("127.0.0.1:0", info, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return s.Addr()
}

func TestDiscovery(t *testing.T) {
	addr := serve(t, Info{HTTPPort: "8096", ID: "cccc0000000000000000000000000001", Name: "blockbustr"})
	for _, q := range []string{"who is JellyfinServer?", "WHO IS EMBYSERVER?"} {
		m, ok := ask(t, addr, q)
		if !ok || m["Address"] != "http://127.0.0.1:8096" || m["Id"] != "cccc0000000000000000000000000001" ||
			m["Name"] != "blockbustr" || m["EndpointAddress"] != nil {
			t.Errorf("%q: %v", q, m)
		}
		if _, present := m["EndpointAddress"]; !present {
			t.Errorf("EndpointAddress is sent (null), as Jellyfin does")
		}
	}
	if m, ok := ask(t, addr, "hello?"); ok {
		t.Errorf("answered a non-query: %v", m)
	}
}

func TestDiscoveryExternalURL(t *testing.T) {
	addr := serve(t, Info{ExternalURL: "https://media.example.ts.net/", HTTPPort: "8096", ID: "x", Name: "n"})
	if m, ok := ask(t, addr, "who is JellyfinServer?"); !ok || m["Address"] != "https://media.example.ts.net" {
		t.Errorf("external url: %v", m)
	}
}

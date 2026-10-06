// Package discovery answers Jellyfin's LAN server discovery (TASKS P2.11,
// DESIGN §11 Q3): clients broadcast "who is JellyfinServer?" (older ones
// "who is EmbyServer?") to UDP 7359, and every server replies with where it
// is: {"Address","Id","Name","EndpointAddress"}.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
)

// Port is the UDP port Jellyfin clients broadcast to.
const Port = 7359

// Info is what the server announces.
type Info struct {
	// ExternalURL, when set, is the Address (server.external_url): the
	// only right answer behind NAT or in a container.
	ExternalURL string
	// HTTPPort completes a derived Address ("http://<local ip>:<port>").
	HTTPPort string
	ID       string // server id, N format
	Name     string
}

// reply is ServerDiscoveryInfo; EndpointAddress is always null, as Jellyfin
// sends it.
type reply struct {
	Address         string
	Id              string //nolint:revive // Jellyfin's field name
	Name            string
	EndpointAddress *string
}

// isQuery reports whether a datagram asks for servers.
func isQuery(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "who is jellyfinserver?") || strings.Contains(m, "who is embyserver?")
}

// localIP is this host's address on the route to remote (no packet sent).
func localIP(remote *net.UDPAddr) net.IP {
	c, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP
}

// address is the URL a client at remote should use.
func (i Info) address(remote *net.UDPAddr) string {
	if i.ExternalURL != "" {
		return strings.TrimRight(i.ExternalURL, "/")
	}
	ip := localIP(remote)
	if ip == nil {
		return ""
	}
	return "http://" + net.JoinHostPort(ip.String(), i.HTTPPort)
}

// Server is a bound discovery responder.
type Server struct {
	conn *net.UDPConn
	info Info
	log  *slog.Logger
}

// Listen binds addr (":7359" in production).
func Listen(addr string, info Info, log *slog.Logger) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	return &Server{conn: c, info: info, log: log}, nil
}

// Addr is the bound address.
func (s *Server) Addr() net.Addr { return s.conn.LocalAddr() }

// Serve answers queries until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.conn.Close()
	}()
	buf := make([]byte, 1024)
	for {
		n, remote, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		if !isQuery(string(buf[:n])) {
			continue
		}
		addr := s.info.address(remote)
		if addr == "" {
			continue
		}
		data, _ := json.Marshal(reply{Address: addr, Id: s.info.ID, Name: s.info.Name})
		if _, err := s.conn.WriteToUDP(data, remote); err != nil {
			s.log.Debug("discovery reply failed", "to", remote, "err", err)
		}
	}
}

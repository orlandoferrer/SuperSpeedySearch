// Package discovery handles LAN mDNS advertisement and browsing.
// mDNS is one way to find nodes, not the center of the architecture:
// manual node URLs must always work without it.
package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/grandcat/zeroconf"
)

const Service = "_superspeedysearch._tcp"

// Advertise registers the node on the LAN until ctx is cancelled.
func Advertise(ctx context.Context, nodeID, name, listenAddr, version string, authRequired bool) error {
	_, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("parse listen_addr: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("parse listen_addr port: %w", err)
	}
	txt := []string{
		"id=" + nodeID,
		"name=" + name,
		"version=" + version,
		"auth=" + strconv.FormatBool(authRequired),
	}
	server, err := zeroconf.Register(nodeID, Service, "local.", port, txt, nil)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()
	return nil
}

// Node is a discovered peer.
type Node struct {
	Instance string   `json:"instance"`
	Host     string   `json:"host"`
	Addrs    []string `json:"addrs"`
	Port     int      `json:"port"`
	TXT      []string `json:"txt"`
}

// URL returns a best-effort base URL for the node.
func (n Node) URL() string {
	if len(n.Addrs) > 0 {
		return fmt.Sprintf("http://%s:%d", n.Addrs[0], n.Port)
	}
	return fmt.Sprintf("http://%s:%d", n.Host, n.Port)
}

// Browse collects nodes advertised on the LAN for the given duration.
func Browse(ctx context.Context, timeout time.Duration) ([]Node, error) {
	// IPv4-only: the dual-stack resolver is unreliable on macOS, where the
	// IPv6 multicast socket can silently break receiving entirely.
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4))
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := resolver.Browse(ctx, Service, "local.", entries); err != nil {
		return nil, err
	}
	var out []Node
	for e := range entries {
		n := Node{Instance: e.Instance, Host: e.HostName, Port: e.Port, TXT: e.Text}
		for _, ip := range e.AddrIPv4 {
			n.Addrs = append(n.Addrs, ip.String())
		}
		for _, ip := range e.AddrIPv6 {
			n.Addrs = append(n.Addrs, ip.String())
		}
		out = append(out, n)
	}
	return out, nil
}

package peer

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	mdnsService = "_p2pci._tcp"
	mdnsDomain  = "local."
)

// Discovery handles mDNS peer announcement and discovery.
type Discovery struct {
	instanceName string
	peerPort     int
	onPeer       func(addr string)
	onPeerLost   func(addr string)
	log          *slog.Logger

	mu     sync.Mutex
	server *zeroconf.Server
	cancel context.CancelFunc
}

// NewDiscovery creates a discovery handler.
func NewDiscovery(peerPort int, onPeer, onPeerLost func(string), log *slog.Logger) (*Discovery, error) {
	hostname, err := getHostname()
	if err != nil {
		return nil, fmt.Errorf("get hostname: %w", err)
	}

	return &Discovery{
		instanceName: fmt.Sprintf("p2pci-%s-%d", hostname, peerPort),
		peerPort:     peerPort,
		onPeer:       onPeer,
		onPeerLost:   onPeerLost,
		log:          log,
	}, nil
}

// Start registers this node via mDNS and begins browsing for peers.
func (d *Discovery) Start() error {
	server, err := zeroconf.Register(
		d.instanceName,
		mdnsService,
		mdnsDomain,
		d.peerPort,
		[]string{"version=1"},
		nil,
	)
	if err != nil {
		return fmt.Errorf("mDNS register: %w", err)
	}

	d.mu.Lock()
	d.server = server
	d.mu.Unlock()

	d.log.Info("mDNS registered",
		"name", d.instanceName,
		"service", mdnsService,
		"port", d.peerPort,
	)

	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	go d.browseLoop(ctx)
	return nil
}

// Stop deregisters this node and stops browsing.
func (d *Discovery) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
	}
	if d.server != nil {
		d.server.Shutdown()
	}
	d.log.Info("mDNS stopped")
}

// browseLoop continuously scans for peers every 30 seconds.
func (d *Discovery) browseLoop(ctx context.Context) {
	d.doBrowse(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
			d.doBrowse(ctx)
		}
	}
}

func (d *Discovery) doBrowse(ctx context.Context) {
	// grandcat/zeroconf API: NewResolver → resolver.Browse
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		d.log.Warn("mDNS resolver create failed", "err", err)
		return
	}

	entries := make(chan *zeroconf.ServiceEntry, 16)

	browseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := resolver.Browse(browseCtx, mdnsService, mdnsDomain, entries); err != nil {
		if browseCtx.Err() == nil {
			d.log.Warn("mDNS browse error", "err", err)
		}
		return
	}

	// Drain entries until the browse context times out.
	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				return
			}
			if entry.Instance == d.instanceName {
				continue // skip ourselves
			}
			ip := pickIP(entry.AddrIPv4)
			if ip == "" {
				ip = pickIPv6(entry.AddrIPv6)
			}
			if ip == "" {
				continue
			}
			addr := fmt.Sprintf("%s:%d", ip, entry.Port)
			d.log.Info("peer discovered via mDNS",
				"name", entry.Instance,
				"addr", addr,
			)
			d.onPeer(addr)

		case <-browseCtx.Done():
			return
		}
	}
}

func getHostname() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "node", nil
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() {
					return sanitise(ipnet.IP.String()), nil
				}
			}
		}
	}
	return "node", nil
}

func sanitise(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			out = append(out, c)
		} else if c >= 'A' && c <= 'Z' {
			out = append(out, c+32)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}

func pickIP(addrs []net.IP) string {
	for _, ip := range addrs {
		if ip.IsLoopback() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

func pickIPv6(addrs []net.IP) string {
	for _, ip := range addrs {
		if !ip.IsLoopback() {
			return fmt.Sprintf("[%s]", ip.String())
		}
	}
	return ""
}
package upstreamproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// UDPRelayConfig configures a UDPRelay: forward UDP traffic aimed at
// LocalIP:Ports[i] to TargetHost:TargetPorts[i] (and back), through a
// SOCKS5 proxy's UDP ASSOCIATE. The relay mechanism itself (SOCKS5
// handshake, UDP ASSOCIATE, bidirectional datagram forwarding) is
// validated and generically correct -- see spike/udprelayunit for a real
// bidirectional round trip (client -> relay -> proxy -> echo server ->
// proxy -> relay -> client) through a real local SOCKS5 proxy (dante).
//
// # Known dead end: fronting a same-host charon's own IKEv2 traffic
//
// This does NOT work for making a same-host charon's SWu tunnel go through
// a proxy by having this relay present a local "fake ePDG" address on
// ports 500/4500 for charon to dial -- confirmed by reading strongSwan's
// own source (src/libcharon/network/receiver.c,
// src/libcharon/plugins/socket_default/socket_default_socket.c), not by
// assumption:
//
//  1. charon's socket-default plugin always binds 0.0.0.0 (wildcard) on
//     BOTH its IKE port and its NAT-T port at startup (open_socketpair),
//     regardless of charon.port/port_nat_t and regardless of whether any
//     connection ever actually floats to NAT-T. A wildcard bind on a port
//     claims that port on every local address, so nothing else on the
//     same host can bind port 500 or 4500 on ANY address while charon is
//     up -- moving charon off those ports (engine/swu/charon.Options.Port/
//     PortNATT) is the only way to free them for a relay.
//  2. But moving charon off 500/4500 breaks its own receiver: charon's
//     receive path (receiver.c's receive_packets) treats a packet as
//     ambiguous (could be ESP, could be IKE with a non-ESP marker) unless
//     EITHER its source OR its destination port is exactly 500 (the
//     IKEV2_UDP_PORT constant, not derived from charon.port). A first-ever
//     IKE_SA_INIT request has no non-ESP marker, so once charon's own port
//     is moved and the packet arrives via a proxy hop that used some other
//     ephemeral source port (which real SOCKS5 proxies do, confirmed
//     against dante), BOTH ports are non-500 and charon silently hands the
//     packet to its ESP callback instead of processing it as IKE --
//     confirmed with tcpdump (the packet physically arrives) plus a
//     bumped charon.filelog net level (nothing after "received packet";
//     no IKE processing is ever logged).
//  3. Even ignoring (2), IKEv2's NAT-T float target is hardcoded to port
//     4500 (IKEV2_NATT_PORT) on the peer side regardless of any
//     remote_port config, so a relay fronting the post-float exchange
//     would need port 4500 specifically too -- which is exactly the port
//     item (1) already says can't be freed without moving charon, which
//     item (2) says breaks receiving.
//
// None of this applies to fronting a genuinely remote peer (a real ePDG on
// a different machine never competes for this host's own port 500/4500,
// and its own receiver never sees our local port juggling) -- only to
// presenting a same-host relay as a stand-in for one, which would need
// real network-namespace isolation (a separate IP stack for charon, e.g.
// via a veth pair) to avoid both problems at once. That hasn't been built.
type UDPRelayConfig struct {
	ProxyAddr string
	Username  string
	Password  string

	// TargetHost is the real peer's address (e.g. an ePDG's FQDN or IP).
	// Resolved once, locally, when the relay starts -- resolution itself is
	// NOT proxied, only the UDP payloads are.
	TargetHost string

	// LocalIP is where the relay listens, once per entry in Ports.
	LocalIP string
	Ports   []int

	// TargetPorts maps each Ports[i] to the port to forward it to on
	// TargetHost. Defaults to Ports itself (the normal case: a real ePDG
	// listens on the same 500/4500 the relay presents) if left nil --
	// set explicitly only when the listen and forward-to ports must
	// differ, e.g. to work around the same-host conflict described above.
	TargetPorts []int
}

// UDPRelay is a running relay; call Close to tear it down.
type UDPRelay struct {
	ctrl      net.Conn // SOCKS5 TCP control connection; keeps the UDP ASSOCIATE alive
	toRelay   *net.UDPConn
	relayAddr *net.UDPAddr
	targetIP  net.IP
	listeners []*net.UDPConn

	portMap        map[int]int // listen port -> target port
	reversePortMap map[int]int // target port -> listen port

	mu    sync.Mutex
	peers map[int]*net.UDPAddr // listen port -> last-seen local peer (e.g. charon) to reply to

	cancel context.CancelFunc
	done   chan struct{}
}

// StartUDPRelay dials cfg.ProxyAddr, performs the SOCKS5 handshake and UDP
// ASSOCIATE (reusing probeHandshake/probeUDPAssociate's exact wire logic --
// already validated by ProbeSOCKS5), then binds cfg.LocalIP on each of
// cfg.Ports and starts pumping datagrams in both directions.
func StartUDPRelay(ctx context.Context, cfg UDPRelayConfig) (*UDPRelay, error) {
	proxyAddr := strings.TrimSpace(cfg.ProxyAddr)
	if proxyAddr == "" {
		return nil, errors.New("upstreamproxy: UDPRelayConfig.ProxyAddr is required")
	}
	targetHost := strings.TrimSpace(cfg.TargetHost)
	if targetHost == "" {
		return nil, errors.New("upstreamproxy: UDPRelayConfig.TargetHost is required")
	}
	localIP := strings.TrimSpace(cfg.LocalIP)
	if localIP == "" {
		return nil, errors.New("upstreamproxy: UDPRelayConfig.LocalIP is required")
	}
	if len(cfg.Ports) == 0 {
		return nil, errors.New("upstreamproxy: UDPRelayConfig.Ports is required")
	}
	targetPorts := cfg.TargetPorts
	if targetPorts == nil {
		targetPorts = cfg.Ports
	}
	if len(targetPorts) != len(cfg.Ports) {
		return nil, errors.New("upstreamproxy: UDPRelayConfig.TargetPorts must be the same length as Ports")
	}
	portMap := make(map[int]int, len(cfg.Ports))
	reversePortMap := make(map[int]int, len(cfg.Ports))
	for i, p := range cfg.Ports {
		portMap[p] = targetPorts[i]
		reversePortMap[targetPorts[i]] = p
	}

	targetIPs, err := net.DefaultResolver.LookupIP(ctx, "ip4", targetHost)
	if err != nil || len(targetIPs) == 0 {
		return nil, fmt.Errorf("upstreamproxy: resolve target %s: %w", targetHost, err)
	}
	targetIP := targetIPs[0]

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	ctrl, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("upstreamproxy: dial proxy %s: %w", proxyAddr, err)
	}
	if err := ctrl.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("upstreamproxy: set handshake deadline: %w", err)
	}
	if _, err := probeHandshake(ctrl, cfg.Username, cfg.Password); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("upstreamproxy: socks5 handshake: %w", err)
	}
	relayAddr, err := probeUDPAssociate(ctrl)
	if err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("upstreamproxy: socks5 udp associate: %w", err)
	}
	// The UDP ASSOCIATE session lives as long as ctrl stays open (RFC 1928
	// §7); this relay is meant to run indefinitely, so the handshake-only
	// deadline is cleared now that setup succeeded.
	if err := ctrl.SetDeadline(time.Time{}); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("upstreamproxy: clear deadline: %w", err)
	}

	toRelay, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("upstreamproxy: dial relay udp endpoint %s: %w", relayAddr, err)
	}

	relayCtx, cancel := context.WithCancel(context.Background())
	r := &UDPRelay{
		ctrl:           ctrl,
		toRelay:        toRelay,
		relayAddr:      relayAddr,
		targetIP:       targetIP,
		portMap:        portMap,
		reversePortMap: reversePortMap,
		peers:          make(map[int]*net.UDPAddr),
		cancel:         cancel,
		done:           make(chan struct{}),
	}

	for _, port := range cfg.Ports {
		ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(localIP), Port: port})
		if err != nil {
			r.closeAll()
			return nil, fmt.Errorf("upstreamproxy: listen %s:%d: %w", localIP, port, err)
		}
		r.listeners = append(r.listeners, ln)
	}

	var wg sync.WaitGroup
	wg.Add(len(r.listeners) + 1)
	for i, ln := range r.listeners {
		go func(ln *net.UDPConn, port int) {
			defer wg.Done()
			r.pumpLocalToRelay(relayCtx, ln, port)
		}(ln, cfg.Ports[i])
	}
	go func() {
		defer wg.Done()
		r.pumpRelayToLocal(relayCtx)
	}()
	go func() {
		wg.Wait()
		close(r.done)
	}()

	return r, nil
}

// Close tears down every listener, the relay-facing socket, and the SOCKS5
// control connection (which ends the UDP ASSOCIATE session server-side).
func (r *UDPRelay) Close() error {
	r.cancel()
	r.closeAll()
	<-r.done
	return nil
}

func (r *UDPRelay) closeAll() {
	for _, ln := range r.listeners {
		ln.Close()
	}
	if r.toRelay != nil {
		r.toRelay.Close()
	}
	if r.ctrl != nil {
		r.ctrl.Close()
	}
}

// pumpLocalToRelay reads datagrams arriving at a local listener (from
// charon), remembers the sender so replies can be routed back, and
// forwards each one to the SOCKS5 relay wrapped in the UDP request header
// (RFC 1928 §7) addressed at targetIP:targetPort (portMap[port], usually
// == port -- see UDPRelayConfig.TargetPorts doc comment for the one case
// it isn't).
func (r *UDPRelay) pumpLocalToRelay(ctx context.Context, ln *net.UDPConn, port int) {
	targetPort := r.portMap[port]
	buf := make([]byte, 65535)
	for {
		n, peer, err := ln.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		r.mu.Lock()
		r.peers[port] = peer
		r.mu.Unlock()

		packet := encodeSOCKS5UDPHeader(r.targetIP, targetPort, buf[:n])
		if _, err := r.toRelay.Write(packet); err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
	}
}

// pumpRelayToLocal reads datagrams coming back from the SOCKS5 relay
// (each still wrapped in its own UDP request header identifying which
// target port the payload came from), strips the header, maps that target
// port back to the listen port that forwards to it, and delivers the
// payload to that listener -- replying to the last-seen peer on it.
func (r *UDPRelay) pumpRelayToLocal(ctx context.Context) {
	buf := make([]byte, 65535)
	for {
		n, err := r.toRelay.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		targetPort, payload, err := decodeSOCKS5UDPHeader(buf[:n])
		if err != nil {
			continue
		}
		listenPort, ok := r.reversePortMap[targetPort]
		if !ok {
			continue // response from a port we never sent to -- can't have a peer for it
		}

		r.mu.Lock()
		peer := r.peers[listenPort]
		r.mu.Unlock()
		if peer == nil {
			continue // nothing has sent from this local port yet -- nowhere to deliver
		}

		for _, ln := range r.listeners {
			if ln.LocalAddr().(*net.UDPAddr).Port == listenPort {
				_, _ = ln.WriteToUDP(payload, peer)
				break
			}
		}
	}
}

// encodeSOCKS5UDPHeader wraps payload per RFC 1928 §7: RSV(2) FRAG(1)
// ATYP(1) DST.ADDR DST.PORT(2) DATA.
func encodeSOCKS5UDPHeader(ip net.IP, port int, payload []byte) []byte {
	v4 := ip.To4()
	header := make([]byte, 0, 10+len(payload))
	header = append(header, 0x00, 0x00, 0x00, socks5AtypIPv4)
	header = append(header, v4...)
	header = append(header, byte(port>>8), byte(port))
	return append(header, payload...)
}

// decodeSOCKS5UDPHeader reverses encodeSOCKS5UDPHeader, returning the
// encoded port (used here as the local-listener key, since the relay always
// echoes back whichever address/port the datagram came from) and the
// stripped payload.
func decodeSOCKS5UDPHeader(data []byte) (port int, payload []byte, err error) {
	if len(data) < 4 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	atyp := data[3]
	rest := data[4:]
	var addrLen int
	switch atyp {
	case socks5AtypIPv4:
		addrLen = 4
	case socks5AtypIPv6:
		addrLen = 16
	default:
		return 0, nil, fmt.Errorf("unsupported ATYP 0x%02x", atyp)
	}
	if len(rest) < addrLen+2 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	portBuf := rest[addrLen : addrLen+2]
	port = int(portBuf[0])<<8 | int(portBuf[1])
	return port, rest[addrLen+2:], nil
}

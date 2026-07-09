// Command vohive-relay is a standalone IKEv2 UDP relay: it binds ports 500
// and 4500 (plain, no port juggling needed) and forwards everything to a
// real target host through a SOCKS5 proxy's UDP ASSOCIATE.
//
// This exists specifically to run on a DIFFERENT machine than the one
// running charon (this vohive's SWu tunnel) -- see
// internal/upstreamproxy.UDPRelay's doc comment for why that's a hard
// requirement: charon always wildcard-binds both its own ports, so a relay
// on the SAME host can never claim them back, no matter how the ports are
// juggled. Running on a separate host sidesteps that entirely: there's no
// charon here to conflict with.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/1239t/vohive/internal/upstreamproxy"
)

func main() {
	proxyAddr := flag.String("proxy", "", "SOCKS5 proxy address, host:port")
	username := flag.String("user", "", "SOCKS5 username (optional)")
	password := flag.String("pass", "", "SOCKS5 password (optional)")
	targetHost := flag.String("target", "", "real peer to forward to (e.g. an ePDG FQDN or IP)")
	bindIP := flag.String("bind", "0.0.0.0", "local address to bind ports 500/4500 on")
	flag.Parse()

	if *proxyAddr == "" || *targetHost == "" {
		fmt.Println("usage: vohive-relay -proxy host:port -target epdg.example.com [-user U -pass P] [-bind 0.0.0.0]")
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	relay, err := upstreamproxy.StartUDPRelay(startCtx, upstreamproxy.UDPRelayConfig{
		ProxyAddr:  *proxyAddr,
		Username:   *username,
		Password:   *password,
		TargetHost: *targetHost,
		LocalIP:    *bindIP,
		Ports:      []int{500, 4500},
	})
	if err != nil {
		log.Fatalf("start relay: %v", err)
	}
	defer relay.Close()

	log.Printf("relay up: %s:{500,4500} -> (via %s) -> %s:{500,4500}", *bindIP, *proxyAddr, *targetHost)
	<-ctx.Done()
	log.Println("shutting down")
}

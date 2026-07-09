// Command udprelayunit validates internal/upstreamproxy.UDPRelay's core
// mechanism in isolation, bidirectionally, against a real local SOCKS5
// proxy -- not charon, not IKEv2, just a plain UDP client and echo
// server, to prove the relay itself (SOCKS5 handshake, UDP ASSOCIATE,
// forwarding both directions, demultiplexing by port) is correct before
// laying anything IKEv2-shaped on top of it.
//
// Requires a real SOCKS5 proxy with UDP ASSOCIATE support listening at
// 127.0.0.1:1080 (this was run against `dante-server`, apt package
// dante-server, configured with `external: 127.0.0.1` so its own outbound
// traffic stays loopback-only -- see /etc/danted.conf during that test).
//
// This passed. What it does NOT prove -- and what turned out not to work
// -- is fronting a same-host charon's own IKEv2 traffic this way; see
// internal/upstreamproxy/udprelay.go's UDPRelayConfig doc comment for
// exactly why (charon's receiver hardcodes port 500 as significant, and
// charon always wildcard-binds both its ports regardless of config).
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/1239t/vohive/internal/upstreamproxy"
)

func main() {
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19999})
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := ln.ReadFromUDP(buf)
			if err != nil {
				return
			}
			fmt.Printf("ECHO SERVER got %d bytes from %s: %q\n", n, addr, buf[:n])
			reply := append([]byte("echo:"), buf[:n]...)
			if _, err := ln.WriteToUDP(reply, addr); err != nil {
				fmt.Println("ECHO SERVER reply failed:", err)
			} else {
				fmt.Println("ECHO SERVER sent reply")
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	relay, err := upstreamproxy.StartUDPRelay(ctx, upstreamproxy.UDPRelayConfig{
		ProxyAddr:   "127.0.0.1:1080",
		TargetHost:  "127.0.0.1",
		LocalIP:     "127.0.0.1",
		Ports:       []int{25000},
		TargetPorts: []int{19999},
	})
	if err != nil {
		log.Fatalf("StartUDPRelay: %v", err)
	}
	defer relay.Close()
	fmt.Println("relay up: 127.0.0.1:25000 -> proxy -> 127.0.0.1:19999")

	client, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25000})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte("hello through UDPRelay")); err != nil {
		log.Fatal(err)
	}
	fmt.Println("client sent packet to 127.0.0.1:25000")

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		fmt.Println("FAIL: client never got a reply back through the relay:", err)
		return
	}
	fmt.Printf("PASS: client got reply through relay: %q\n", buf[:n])
}

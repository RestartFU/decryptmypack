package downloader

import (
	"context"
	"fmt"
	"net"

	"github.com/sandertv/gophertunnel/minecraft"
)

// AnonymousRakNet ...
type AnonymousRakNet struct {
	minecraft.RakNet

	proxies []proxyInfo
}

// NewAnonymousRakNet ...
func NewAnonymousRakNet(proxies []proxyInfo) *AnonymousRakNet {
	return &AnonymousRakNet{proxies: proxies}
}

// DialContext ...
func (a *AnonymousRakNet) DialContext(ctx context.Context, address string) (net.Conn, error) {
	client := newClientWithProxy(randomProxy(a.proxies))
	c, err := client.Dial("udp", address)
	if err != nil {
		fmt.Println(err)
	}
	return c, err
}

// Dial ...
func (a *AnonymousRakNet) Dial(network, address string) (net.Conn, error) {
	client := newClientWithProxy(randomProxy(a.proxies))
	return client.Dial(network, address)
}

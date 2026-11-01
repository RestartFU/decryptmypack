package nettarget

import (
	"errors"
	"net"
	"net/netip"
	"strings"
)

var (
	ErrNoAddresses     = errors.New("host resolved to no IP addresses")
	ErrNonPublicTarget = errors.New("host must resolve only to public IP addresses")
)

// NormalizeHost returns the canonical spelling used when comparing server targets.
func NormalizeHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(host)
}

// ValidHostname reports whether host is a plain DNS hostname accepted as an outbound target.
func ValidHostname(host string) bool {
	host = NormalizeHost(host)
	if host == "" || len(host) > 253 || strings.Contains(host, ":") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i, r := range label {
			letter := r >= 'a' && r <= 'z'
			digit := r >= '0' && r <= '9'
			hyphen := r == '-'
			if !letter && !digit && !hyphen {
				return false
			}
			if hyphen && (i == 0 || i == len(label)-1) {
				return false
			}
		}
	}
	return true
}

// SelectPublicIP validates all addresses before selecting one, preventing a mixed DNS answer from bypassing checks.
func SelectPublicIP(ips []net.IP) (string, error) {
	addresses := make([]net.IPAddr, 0, len(ips))
	for _, ip := range ips {
		addresses = append(addresses, net.IPAddr{IP: ip})
	}
	selected, err := PublicIPStrings(addresses)
	if err != nil {
		return "", err
	}
	return selected[0], nil
}

// PublicIPStrings validates all resolved addresses and returns their canonical strings.
func PublicIPStrings(addresses []net.IPAddr) ([]string, error) {
	if len(addresses) == 0 {
		return nil, ErrNoAddresses
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if !PublicIP(address.IP) {
			return nil, ErrNonPublicTarget
		}
		result = append(result, address.IP.String())
	}
	return result, nil
}

// PublicIP rejects local, private, documentation, transition and otherwise non-routable destinations.
func PublicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

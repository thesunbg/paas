// Package netguard holds the one definition of "a public address" shared by
// every place that dials a tenant-supplied host.
package netguard

import "net"

var _, cgnat, _ = net.ParseCIDR("100.64.0.0/10")

// Public reports whether ip is a globally routable unicast address: not
// loopback, private, link-local, unspecified, multicast or CGNAT.
func Public(ip net.IP) bool {
	return ip != nil && !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip))
}

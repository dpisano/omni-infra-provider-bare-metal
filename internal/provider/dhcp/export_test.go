// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dhcp

import (
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

var (
	IsBootDHCP   = isBootDHCP
	ValidateDHCP = validateDHCP
)

// HandlePacket exposes the packet handler, so the decisions it makes before building a response can
// be driven from a test without standing up a real DHCP listener.
func (p *Proxy) HandlePacket(port int) func(net.PacketConn, net.Addr, *dhcpv4.DHCPv4) {
	return p.handlePacket(port)
}

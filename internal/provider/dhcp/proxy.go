// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/siderolabs/gen/xslices"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// Port67 is the standard DHCP server port, used for broadcast DHCPDISCOVER.
	Port67 = 67
	// Port4011 is the PXE proxy DHCP port, used for unicast DHCPREQUEST.
	Port4011 = 4011
)

// Proxy is a DHCP proxy server, adding PXE boot options to the DHCP responses.
type Proxy struct {
	logger                   *zap.Logger
	apiAdvertiseAddress      string
	proxyIfaceOrIP           string
	apiPort                  int
	disableBroadcastListener bool
}

// NewProxy creates a new DHCP proxy server.
func NewProxy(apiAdvertiseAddress string, apiPort int, proxyIfaceOrIP string, disableBroadcastListener bool, logger *zap.Logger) *Proxy {
	return &Proxy{
		apiAdvertiseAddress:      apiAdvertiseAddress,
		apiPort:                  apiPort,
		proxyIfaceOrIP:           proxyIfaceOrIP,
		disableBroadcastListener: disableBroadcastListener,
		logger:                   logger,
	}
}

// Run starts the DHCP proxy server on port 67 and port 4011.
//
// Per the PXE specification (section 2.5.1.1), the redirection service must be prepared
// to receive DHCPDISCOVER on port 67 and DHCPREQUEST on port 4011.
//
// Port 67 handles the broadcast DHCPDISCOVER from PXE clients and responds with DHCPOFFER.
// Port 4011 handles the unicast DHCPREQUEST from PXE clients that follow the two-phase
// proxy DHCP flow, and responds with DHCPACK.
//
// When disableBroadcastListener is true, only port 4011 is used. This is for deployments
// where the provider runs on the same host as the DHCP server and cannot bind to port 67.
func (p *Proxy) Run(ctx context.Context) error {
	iface, err := p.determineInterface(p.proxyIfaceOrIP)
	if err != nil {
		return fmt.Errorf("failed to determine interface: %w", err)
	}

	eg, ctx := errgroup.WithContext(ctx)

	if !p.disableBroadcastListener {
		p.logger.Info("starting DHCP proxy broadcast listener on port 67")

		eg.Go(func() error {
			return p.runServer(ctx, iface, Port67)
		})
	}

	p.logger.Info("starting DHCP proxy direct listener on port 4011")

	eg.Go(func() error {
		return p.runServer(ctx, iface, Port4011)
	})

	return eg.Wait()
}

func (p *Proxy) runServer(ctx context.Context, iface string, port int) error {
	laddr := &net.UDPAddr{Port: port}

	server, err := server4.NewServer(iface, laddr, p.handlePacket(port))
	if err != nil {
		return fmt.Errorf("failed to create DHCP server on port %d: %w", port, err)
	}

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		if serveErr := server.Serve(); serveErr != nil {
			if errors.Is(serveErr, net.ErrClosed) {
				return nil
			}

			return fmt.Errorf("failed to run DHCP server on port %d: %w", port, serveErr)
		}

		return nil
	})

	eg.Go(func() error {
		<-ctx.Done()

		return server.Close()
	})

	return eg.Wait()
}

func (p *Proxy) determineInterface(ifaceOrIP string) (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("failed to get network interfaces: %w", err)
	}

	targetIP := net.ParseIP(ifaceOrIP)

	for _, iface := range interfaces {
		if iface.Name == ifaceOrIP {
			return iface.Name, nil
		}

		if targetIP == nil { // not an IP address, we are matching only against interfaces, skip
			continue
		}

		addrs, addrsErr := iface.Addrs()
		if addrsErr != nil {
			p.logger.Error("failed to list addresses for interface", zap.String("interface", iface.Name), zap.Error(addrsErr))
		}

		for _, addr := range addrs {
			// Extract IP from address
			var ip net.IP

			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip != nil && ip.Equal(targetIP) {
				return iface.Name, nil
			}
		}
	}

	return "", fmt.Errorf("no interface found for: %s", ifaceOrIP)
}

func (p *Proxy) handlePacket(port int) func(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	return func(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
		logger := p.logger.With(zap.String("source", m.ClientHWAddr.String()), zap.Int("port", port))

		if err := isBootDHCP(m, port); err != nil {
			logger.Debug("ignoring packet", zap.Error(err))

			return
		}

		fwtype, err := validateDHCP(m)
		if err != nil {
			logger.Debug("invalid packet", zap.Error(err))

			return
		}

		resp, err := OfferDHCP(m, p.apiAdvertiseAddress, p.apiPort, fwtype, port)
		if err != nil {
			logger.Error("failed to construct ProxyDHCP response", zap.Error(err))

			return
		}

		logger.Info("offering boot response", zap.String("boot_filename", resp.BootFileNameOption()), zap.String("message_type", resp.MessageType().String()))

		_, err = conn.WriteTo(resp.ToBytes(), peer)
		if err != nil {
			logger.Error("failure sending response", zap.Error(err))
		}
	}
}

// isBootDHCP checks if the packet is a PXE boot DHCP packet appropriate for the given port.
//
// Per the PXE spec (section 2.5.1.1, page 35):
//   - Port 67 receives DHCPDISCOVER (broadcast from PXE clients)
//   - Port 4011 receives DHCPREQUEST (unicast/direct from PXE clients in the two-phase flow)
func isBootDHCP(pkt *dhcpv4.DHCPv4, port int) error {
	var expectedType dhcpv4.MessageType

	switch port {
	case Port4011:
		expectedType = dhcpv4.MessageTypeRequest
	default:
		expectedType = dhcpv4.MessageTypeDiscover
	}

	if pkt.MessageType() != expectedType {
		return fmt.Errorf("packet is %s, not %s", pkt.MessageType(), expectedType)
	}

	if pkt.Options[93] == nil {
		return errors.New("not a PXE boot request (missing option 93)")
	}

	return nil
}

func validateDHCP(m *dhcpv4.DHCPv4) (fwtype Firmware, err error) {
	arches := m.ClientArch()

	for _, arch := range arches {
		switch arch { //nolint:exhaustive
		case iana.INTEL_X86PC:
			fwtype = FirmwareX86PC
		case iana.EFI_IA32, iana.EFI_X86_64, iana.EFI_BC:
			fwtype = FirmwareX86EFI
		case iana.EFI_ARM64, iana.UBOOT_ARM64:
			fwtype = FirmwareARMEFI
		case iana.EFI_X86_HTTP, iana.EFI_X86_64_HTTP:
			fwtype = FirmwareX86HTTP
		case iana.EFI_ARM64_HTTP, iana.UBOOT_ARM64_HTTP:
			fwtype = FirmwareARMHTTP
		}
	}

	if fwtype == FirmwareUnsupported {
		return 0, fmt.Errorf("unsupported client arch: %v", xslices.Map(arches, func(a iana.Arch) string { return a.String() }))
	}

	// The Raspberry Pi bootloader announces itself as a legacy x86 BIOS client (architecture 0),
	// so going by the architecture alone would hand an ARM board an x86 bootloader. Identify it by
	// its MAC address instead, and let it drive its own boot chain from TFTP.
	if fwtype == FirmwareX86PC && isRaspberryPi(m.ClientHWAddr) {
		fwtype = FirmwareRPi
	}

	// Now, identify special sub-breeds of client firmware based on
	// the user-class option. Note these only change the "firmware
	// type", not the architecture we're reporting to Booters. We need
	// to identify these as part of making the internal chainloading
	// logic work properly.
	if userClasses := m.UserClass(); len(userClasses) > 0 {
		// If the client has had iPXE burned into its ROM (or is a VM
		// that uses iPXE as the PXE "ROM"), special handling is
		// needed because in this mode the client is using iPXE native
		// drivers and chainloading to a UNDI stack won't work.
		if userClasses[0] == "iPXE" && fwtype == FirmwareX86PC {
			fwtype = FirmwareX86Ipxe
		}
	}

	guid := m.GetOneOption(dhcpv4.OptionClientMachineIdentifier)
	switch len(guid) {
	case 0:
		// A missing GUID is invalid according to the spec, however
		// there are PXE ROMs in the wild that omit the GUID and still
		// expect to boot. The only thing we do with the GUID is
		// mirror it back to the client if it's there, so we might as
		// well accept these buggy ROMs.
	case 17:
		if guid[0] != 0 {
			return 0, errors.New("malformed client GUID (option 97), leading byte must be zero")
		}
	default:
		return 0, errors.New("malformed client GUID (option 97), wrong size")
	}

	return fwtype, nil
}

// OfferDHCP constructs the DHCP response for a PXE boot request.
//
// On port 67, this is a DHCPOFFER in response to DHCPDISCOVER.
// On port 4011, this is a DHCPACK in response to DHCPREQUEST.
func OfferDHCP(req *dhcpv4.DHCPv4, apiAdvertiseAddress string, apiPort int, fwtype Firmware, port int) (*dhcpv4.DHCPv4, error) {
	serverIP := net.ParseIP(apiAdvertiseAddress)
	ipPort := net.JoinHostPort(serverIP.String(), strconv.Itoa(apiPort))

	var replyType dhcpv4.MessageType

	switch port {
	case Port4011:
		replyType = dhcpv4.MessageTypeAck
	default:
		replyType = dhcpv4.MessageTypeOffer
	}

	modifiers := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(replyType),
		dhcpv4.WithServerIP(serverIP),
		dhcpv4.WithOptionCopied(req, dhcpv4.OptionClientMachineIdentifier),
		dhcpv4.WithOptionCopied(req, dhcpv4.OptionClassIdentifier),
	}

	resp, err := dhcpv4.NewReplyFromRequest(
		req,
		modifiers...,
	)
	if err != nil {
		return nil, err
	}

	if resp.GetOneOption(dhcpv4.OptionClassIdentifier) == nil {
		resp.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient"))
	}

	// For the standard PXE TFTP firmware types below, the boot filename is also written to the BOOTP
	// `file` header field in addition to DHCP option 67. U-Boot's ProxyDHCP code path reads the filename
	// from the header directly and never processes DHCP options, so without this the BOOTP file field
	// would be empty. iPXE and HTTP-boot firmwares are not included here since they process DHCP options
	// correctly.
	switch fwtype {
	case FirmwareX86PC:
		// This is completely standard PXE: just load a file from TFTP.
		resp.UpdateOption(dhcpv4.OptTFTPServerName(serverIP.String()))
		resp.BootFileName = "undionly.kpxe"
		resp.UpdateOption(dhcpv4.OptBootFileName(resp.BootFileName))
	case FirmwareX86Ipxe:
		// Almost standard PXE, but the boot filename needs to be a URL.
		urlHost := serverIP.String()
		if serverIP.To4() == nil { // IPv6 host must be bracketed in a URL authority per RFC 3986
			urlHost = "[" + urlHost + "]"
		}

		resp.UpdateOption(dhcpv4.OptBootFileName(fmt.Sprintf("tftp://%s/undionly.kpxe", urlHost)))
	case FirmwareX86EFI:
		// This is completely standard PXE: just load a file from TFTP.
		resp.UpdateOption(dhcpv4.OptTFTPServerName(serverIP.String()))
		resp.BootFileName = "snp.efi"
		resp.UpdateOption(dhcpv4.OptBootFileName(resp.BootFileName))
	case FirmwareARMEFI:
		// This is completely standard PXE: just load a file from TFTP.
		resp.UpdateOption(dhcpv4.OptTFTPServerName(serverIP.String()))
		resp.BootFileName = "snp-arm64.efi"
		resp.UpdateOption(dhcpv4.OptBootFileName(resp.BootFileName))
	case FirmwareX86HTTP:
		// This is completely standard HTTP-boot: just load a file from HTTP.
		resp.UpdateOption(dhcpv4.OptBootFileName(fmt.Sprintf("http://%s/tftp/amd64/snp.efi", ipPort)))
	case FirmwareARMHTTP:
		// This is completely standard HTTP-boot: just load a file from HTTP.
		resp.UpdateOption(dhcpv4.OptBootFileName(fmt.Sprintf("http://%s/tftp/arm64/snp.efi", ipPort)))
	case FirmwareRPi:
		// The Raspberry Pi bootloader derives the file names it fetches itself and ignores the boot
		// file name, so it only has to be pointed at the TFTP server. It does, however, refuse an
		// offer whose vendor specific information does not carry the "Raspberry Pi Boot" string.
		pxeOptions, pxeErr := raspberryPiBootPXEOptions(serverIP)
		if pxeErr != nil {
			return nil, pxeErr
		}

		resp.UpdateOption(dhcpv4.OptTFTPServerName(serverIP.String()))
		resp.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionVendorSpecificInformation, pxeOptions))
	case FirmwareUnsupported:
		fallthrough
	default:
		return nil, fmt.Errorf("unsupported firmware type %d", fwtype)
	}

	return resp, nil
}

// Firmware describes a kind of firmware attempting to boot.
//
// This should only be used for selecting the right bootloader,
// kernel selection should key off the more generic architecture.
type Firmware int

// The bootloaders that we know how to handle.
const (
	FirmwareUnsupported Firmware = iota // Unsupported
	FirmwareX86PC                       // "Classic" x86 BIOS with PXE/UNDI support
	FirmwareX86EFI                      // EFI x86
	FirmwareARMEFI                      // EFI ARM64
	FirmwareX86Ipxe                     // "Classic" x86 BIOS running iPXE (no UNDI support)
	FirmwareX86HTTP                     // HTTP Boot X86
	FirmwareARMHTTP                     // ARM64 HTTP Boot
	FirmwareRPi                         // Raspberry Pi on-board bootloader
)

// raspberryPiOUIs are the MAC address prefixes assigned to Raspberry Pi.
//
// The Raspberry Pi bootloader cannot be told apart by its DHCP architecture, which is the same 0 a
// legacy x86 BIOS reports, nor safely by its vendor class, which is a generic legacy PXE string
// that real x86 clients send too. The MAC address is what distinguishes it without breaking x86
// BIOS PXE booting. A Pi booting over a USB network adapter is therefore not recognized.
var raspberryPiOUIs = [][3]byte{
	{0xb8, 0x27, 0xeb}, // Raspberry Pi Foundation
	{0xdc, 0xa6, 0x32}, // Raspberry Pi Trading Ltd
	{0xe4, 0x5f, 0x01}, // Raspberry Pi Trading Ltd
	{0xd8, 0x3a, 0xdd}, // Raspberry Pi Trading Ltd
	{0x28, 0xcd, 0xc1}, // Raspberry Pi Trading Ltd
	{0x2c, 0xcf, 0x67}, // Raspberry Pi Ltd
}

func isRaspberryPi(hwAddr net.HardwareAddr) bool {
	if len(hwAddr) < 3 {
		return false
	}

	return slices.Contains(raspberryPiOUIs, [3]byte(hwAddr[:3]))
}

// raspberryPiBootMenuDescription is the string the Raspberry Pi bootloader looks for in the vendor
// specific information before it accepts an offer.
const raspberryPiBootMenuDescription = "Raspberry Pi Boot"

// PXE vendor specific sub-option codes, from the PXE specification.
const (
	pxeDiscoveryControl = 6
	pxeBootServers      = 8
	pxeBootMenu         = 9
	pxeMenuPrompt       = 10
	pxeEnd              = 255
)

// raspberryPiBootPXEOptions builds the PXE boot menu the Raspberry Pi bootloader expects.
//
// It mirrors what dnsmasq emits for `pxe-service=0,"Raspberry Pi Boot"`, which is the configuration
// the Raspberry Pi network boot documentation prescribes, so it both satisfies a strict PXE client
// and carries the string the bootloader itself looks for.
func raspberryPiBootPXEOptions(serverIP net.IP) ([]byte, error) {
	serverIPv4 := serverIP.To4()
	if serverIPv4 == nil {
		return nil, fmt.Errorf("cannot offer a Raspberry Pi boot response over a non-IPv4 address: %s", serverIP)
	}

	description := []byte(raspberryPiBootMenuDescription)

	// boot server list: server type 0, a single address, which is this provider
	bootServers := make([]byte, 0, 3+net.IPv4len)
	bootServers = append(bootServers, 0x00, 0x00, 0x01)
	bootServers = append(bootServers, serverIPv4...)

	// boot menu: server type 0, then the description
	menu := make([]byte, 0, 3+len(description))
	menu = append(menu, 0x00, 0x00, byte(len(description)))
	menu = append(menu, description...)

	// menu prompt: a zero timeout boots the only entry immediately, without waiting for a key press
	prompt := make([]byte, 0, 1+len(description))
	prompt = append(prompt, 0x00)
	prompt = append(prompt, description...)

	options := dhcpv4.Options{
		pxeDiscoveryControl: []byte{0x03}, // use the boot server list, skip multicast and broadcast discovery
		pxeBootServers:      bootServers,
		pxeBootMenu:         menu,
		pxeMenuPrompt:       prompt,
	}

	// Options.Marshal deliberately skips the End option, so terminate the sub-option space here
	return append(options.ToBytes(), pxeEnd), nil
}

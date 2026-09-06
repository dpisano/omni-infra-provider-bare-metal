// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package dhcp_test

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/dhcp"
)

func TestIsBootDHCP(t *testing.T) {
	t.Parallel()

	pxeDiscover := newPXEPacket(t, dhcpv4.MessageTypeDiscover)
	pxeRequest := newPXEPacket(t, dhcpv4.MessageTypeRequest)
	nonPXEDiscover := newNonPXEPacket(t, dhcpv4.MessageTypeDiscover)

	tests := []struct {
		pkt     *dhcpv4.DHCPv4
		name    string
		wantErr string
		port    int
	}{
		{
			name: "port 67 accepts DHCPDISCOVER",
			pkt:  pxeDiscover,
			port: dhcp.Port67,
		},
		{
			name:    "port 67 rejects DHCPREQUEST",
			pkt:     pxeRequest,
			port:    dhcp.Port67,
			wantErr: "not DISCOVER",
		},
		{
			name: "port 4011 accepts DHCPREQUEST",
			pkt:  pxeRequest,
			port: dhcp.Port4011,
		},
		{
			name:    "port 4011 rejects DHCPDISCOVER",
			pkt:     pxeDiscover,
			port:    dhcp.Port4011,
			wantErr: "not REQUEST",
		},
		{
			name:    "port 67 rejects non-PXE DHCPDISCOVER",
			pkt:     nonPXEDiscover,
			port:    dhcp.Port67,
			wantErr: "missing option 93",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := dhcp.IsBootDHCP(tt.pkt, tt.port)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestOfferDHCP(t *testing.T) {
	t.Parallel()

	const (
		apiAddr = "192.168.1.100"
		apiPort = 50042
	)

	t.Run("port 67 responds with DHCPOFFER", func(t *testing.T) {
		t.Parallel()

		req := newPXEPacket(t, dhcpv4.MessageTypeDiscover)
		resp, err := dhcp.OfferDHCP(req, apiAddr, apiPort, dhcp.FirmwareX86EFI, dhcp.Port67)
		require.NoError(t, err)

		assert.Equal(t, dhcpv4.MessageTypeOffer, resp.MessageType())
		assert.Equal(t, "snp.efi", resp.BootFileNameOption())
		assert.Equal(t, "snp.efi", resp.BootFileName)
		assert.NotNil(t, resp.GetOneOption(dhcpv4.OptionClassIdentifier))
	})

	t.Run("port 4011 responds with DHCPACK", func(t *testing.T) {
		t.Parallel()

		req := newPXEPacket(t, dhcpv4.MessageTypeRequest)
		resp, err := dhcp.OfferDHCP(req, apiAddr, apiPort, dhcp.FirmwareX86EFI, dhcp.Port4011)
		require.NoError(t, err)

		assert.Equal(t, dhcpv4.MessageTypeAck, resp.MessageType())
		assert.Equal(t, "snp.efi", resp.BootFileNameOption())
		assert.Equal(t, "snp.efi", resp.BootFileName)
		assert.NotNil(t, resp.GetOneOption(dhcpv4.OptionClassIdentifier))
	})

	t.Run("every response carries a server identifier", func(t *testing.T) {
		t.Parallel()

		// RFC 2131 requires option 54 in a DHCPOFFER and a DHCPACK alike, and strict PXE firmware
		// discards a response without one, which looks like a client that is offered a boot file
		// and then never fetches it.
		for _, port := range []int{dhcp.Port67, dhcp.Port4011} {
			msgType := dhcpv4.MessageTypeDiscover
			if port == dhcp.Port4011 {
				msgType = dhcpv4.MessageTypeRequest
			}

			for _, fwtype := range []dhcp.Firmware{
				dhcp.FirmwareX86PC,
				dhcp.FirmwareX86EFI,
				dhcp.FirmwareARMEFI,
				dhcp.FirmwareX86Ipxe,
				dhcp.FirmwareX86HTTP,
				dhcp.FirmwareARMHTTP,
			} {
				resp, err := dhcp.OfferDHCP(newPXEPacket(t, msgType), apiAddr, apiPort, fwtype, port)
				require.NoError(t, err)

				assert.Equal(t, net.ParseIP(apiAddr).To4(), resp.ServerIdentifier().To4(),
					"firmware type %d on port %d", fwtype, port)
			}
		}

		// A proxy response is told apart from a real one by carrying no address for the client,
		// so the server identifier must not be mistaken for an address offer.
		resp, err := dhcp.OfferDHCP(newPXEPacket(t, dhcpv4.MessageTypeDiscover), apiAddr, apiPort, dhcp.FirmwareX86EFI, dhcp.Port67)
		require.NoError(t, err)

		assert.True(t, resp.YourIPAddr.IsUnspecified(), "a proxy offer must not hand out an address")
	})

	t.Run("an IPv6 advertise address leaves the server identifier out rather than encoding it empty", func(t *testing.T) {
		t.Parallel()

		// Option 54 is four bytes wide, and only the URL-based firmware types are reachable over
		// IPv6 at all, none of which need it.
		resp, err := dhcp.OfferDHCP(newPXEPacket(t, dhcpv4.MessageTypeDiscover), "2001:db8::1", apiPort, dhcp.FirmwareX86HTTP, dhcp.Port67)
		require.NoError(t, err)

		assert.Nil(t, resp.GetOneOption(dhcpv4.OptionServerIdentifier))
	})

	t.Run("TFTP responses tell the client to boot the file they name", func(t *testing.T) {
		t.Parallel()

		// Strict PXE firmware discards a boot server reply whose vendor specific information does
		// not say what to do next, which looks like a client that is offered a boot file and then
		// never fetches it.
		for _, fwtype := range []dhcp.Firmware{dhcp.FirmwareX86PC, dhcp.FirmwareX86EFI, dhcp.FirmwareARMEFI} {
			resp, err := dhcp.OfferDHCP(newPXEPacket(t, dhcpv4.MessageTypeDiscover), apiAddr, apiPort, fwtype, dhcp.Port67)
			require.NoError(t, err)

			vendorOpts := resp.GetOneOption(dhcpv4.OptionVendorSpecificInformation)
			require.NotEmpty(t, vendorOpts, "firmware type %d", fwtype)

			// the sub-option space must be well formed, and terminated with the End option
			assert.Equal(t, byte(255), vendorOpts[len(vendorOpts)-1], "firmware type %d", fwtype)

			parsed := dhcpv4.Options{}
			require.NoError(t, parsed.FromBytes(vendorOpts))

			// discovery control bit 3: boot the file named in this reply, do not discover further
			assert.Equal(t, []byte{0x08}, parsed.Get(dhcpv4.GenericOptionCode(6)), "firmware type %d", fwtype)
		}
	})

	t.Run("responses that carry a boot URL are left alone", func(t *testing.T) {
		t.Parallel()

		// These read the boot file name straight out of the options, and HTTP boot is not a PXE
		// boot server exchange at all, so PXE vendor options have no place in either.
		for _, fwtype := range []dhcp.Firmware{dhcp.FirmwareX86Ipxe, dhcp.FirmwareX86HTTP, dhcp.FirmwareARMHTTP} {
			resp, err := dhcp.OfferDHCP(newPXEPacket(t, dhcpv4.MessageTypeDiscover), apiAddr, apiPort, fwtype, dhcp.Port67)
			require.NoError(t, err)

			assert.Nil(t, resp.GetOneOption(dhcpv4.OptionVendorSpecificInformation), "firmware type %d", fwtype)
		}
	})

	t.Run("firmware types produce correct boot filenames", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			wantFile      string
			wantBOOTPFile string // BOOTP `file` header (resp.BootFileName); empty when only DHCP option 67 is set.
			fwtype        dhcp.Firmware
		}{
			{fwtype: dhcp.FirmwareX86PC, wantFile: "undionly.kpxe", wantBOOTPFile: "undionly.kpxe"},
			{fwtype: dhcp.FirmwareX86EFI, wantFile: "snp.efi", wantBOOTPFile: "snp.efi"},
			{fwtype: dhcp.FirmwareARMEFI, wantFile: "snp-arm64.efi", wantBOOTPFile: "snp-arm64.efi"},
			{fwtype: dhcp.FirmwareX86Ipxe, wantFile: "tftp://192.168.1.100/undionly.kpxe"},
			{fwtype: dhcp.FirmwareX86HTTP, wantFile: "http://192.168.1.100:50042/tftp/amd64/snp.efi"},
			{fwtype: dhcp.FirmwareARMHTTP, wantFile: "http://192.168.1.100:50042/tftp/arm64/snp.efi"},
		}

		for _, tt := range tests {
			req := newPXEPacket(t, dhcpv4.MessageTypeDiscover)
			resp, err := dhcp.OfferDHCP(req, apiAddr, apiPort, tt.fwtype, dhcp.Port67)
			require.NoError(t, err)

			assert.Equal(t, tt.wantFile, resp.BootFileNameOption(), "firmware type %d option 67", tt.fwtype)
			assert.Equal(t, tt.wantBOOTPFile, resp.BootFileName, "firmware type %d BOOTP file header", tt.fwtype)
		}
	})

	t.Run("URL-based boot filenames bracket IPv6 advertise addresses", func(t *testing.T) {
		t.Parallel()

		const ipv6Addr = "2001:db8::1"

		tests := []struct {
			wantFile string
			fwtype   dhcp.Firmware
		}{
			{fwtype: dhcp.FirmwareX86Ipxe, wantFile: "tftp://[2001:db8::1]/undionly.kpxe"},
			{fwtype: dhcp.FirmwareX86HTTP, wantFile: "http://[2001:db8::1]:50042/tftp/amd64/snp.efi"},
			{fwtype: dhcp.FirmwareARMHTTP, wantFile: "http://[2001:db8::1]:50042/tftp/arm64/snp.efi"},
		}

		for _, tt := range tests {
			req := newPXEPacket(t, dhcpv4.MessageTypeDiscover)
			resp, err := dhcp.OfferDHCP(req, ipv6Addr, apiPort, tt.fwtype, dhcp.Port67)
			require.NoError(t, err)

			assert.Equal(t, tt.wantFile, resp.BootFileNameOption(), "firmware type %d", tt.fwtype)
		}
	})
}

func newPXEPacket(t *testing.T, msgType dhcpv4.MessageType) *dhcpv4.DHCPv4 {
	t.Helper()

	pkt, err := dhcpv4.New(
		dhcpv4.WithMessageType(msgType),
		dhcpv4.WithHwAddr([]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}),
	)
	require.NoError(t, err)

	// Option 93: Client System Architecture (EFI x86_64)
	pkt.UpdateOption(dhcpv4.OptClientArch(iana.EFI_X86_64))

	return pkt
}

func newNonPXEPacket(t *testing.T, msgType dhcpv4.MessageType) *dhcpv4.DHCPv4 {
	t.Helper()

	pkt, err := dhcpv4.New(
		dhcpv4.WithMessageType(msgType),
		dhcpv4.WithHwAddr([]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}),
	)
	require.NoError(t, err)

	return pkt
}

// piMAC is a Raspberry Pi Trading Ltd MAC address, as a Pi is recognized by its OUI.
var piMAC = []byte{0xdc, 0xa6, 0x32, 0x11, 0x22, 0x33}

func newRPiPacket(t *testing.T, hwAddr []byte) *dhcpv4.DHCPv4 {
	t.Helper()

	pkt, err := dhcpv4.New(
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDiscover),
		dhcpv4.WithHwAddr(hwAddr),
	)
	require.NoError(t, err)

	// The Raspberry Pi bootloader reports architecture 0, the same a legacy x86 BIOS reports,
	// and a vendor class that real x86 PXE clients also send.
	pkt.UpdateOption(dhcpv4.OptClientArch(iana.INTEL_X86PC))
	pkt.UpdateOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00000:UNDI:002001"))

	return pkt
}

func TestValidateDHCPRaspberryPi(t *testing.T) {
	t.Parallel()

	t.Run("a Raspberry Pi is told apart from a legacy x86 BIOS by its MAC", func(t *testing.T) {
		t.Parallel()

		fwtype, err := dhcp.ValidateDHCP(newRPiPacket(t, piMAC))
		require.NoError(t, err)

		assert.Equal(t, dhcp.FirmwareRPi, fwtype)
	})

	t.Run("an x86 BIOS client with the same architecture is untouched", func(t *testing.T) {
		t.Parallel()

		fwtype, err := dhcp.ValidateDHCP(newRPiPacket(t, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}))
		require.NoError(t, err)

		assert.Equal(t, dhcp.FirmwareX86PC, fwtype)
	})

	t.Run("a Raspberry Pi OUI does not override a real architecture", func(t *testing.T) {
		t.Parallel()

		// a Pi chainloaded into UEFI reports arm64 properly, and must keep getting the arm64 bootloader
		pkt := newRPiPacket(t, piMAC)
		pkt.UpdateOption(dhcpv4.OptClientArch(iana.EFI_ARM64))

		fwtype, err := dhcp.ValidateDHCP(pkt)
		require.NoError(t, err)

		assert.Equal(t, dhcp.FirmwareARMEFI, fwtype)
	})
}

func TestOfferDHCPRaspberryPi(t *testing.T) {
	t.Parallel()

	const (
		apiAddr = "192.168.1.100"
		apiPort = 50042
	)

	t.Run("the offer points at TFTP and carries the Raspberry Pi Boot string", func(t *testing.T) {
		t.Parallel()

		resp, err := dhcp.OfferDHCP(newRPiPacket(t, piMAC), apiAddr, apiPort, dhcp.FirmwareRPi, dhcp.Port67)
		require.NoError(t, err)

		assert.Equal(t, apiAddr, resp.TFTPServerName())

		// the bootloader derives the file names itself, so no boot file is offered
		assert.Empty(t, resp.BootFileNameOption())
		assert.Empty(t, resp.BootFileName)

		vendorOpts := resp.GetOneOption(dhcpv4.OptionVendorSpecificInformation)
		require.NotEmpty(t, vendorOpts, "the Raspberry Pi bootloader refuses an offer without vendor specific information")

		assert.Contains(t, string(vendorOpts), "Raspberry Pi Boot")

		// the sub-option space must be well formed, and terminated with the End option
		assert.Equal(t, byte(255), vendorOpts[len(vendorOpts)-1])

		parsed := dhcpv4.Options{}
		require.NoError(t, parsed.FromBytes(vendorOpts))

		// the boot server list must point back at this provider
		assert.Equal(t, append([]byte{0x00, 0x00, 0x01}, net.ParseIP(apiAddr).To4()...), parsed.Get(dhcpv4.GenericOptionCode(8)))
	})

	t.Run("an IPv6 advertise address is rejected rather than silently mis-encoded", func(t *testing.T) {
		t.Parallel()

		_, err := dhcp.OfferDHCP(newRPiPacket(t, piMAC), "2001:db8::1", apiPort, dhcp.FirmwareRPi, dhcp.Port67)
		require.Error(t, err)

		assert.Contains(t, err.Error(), "non-IPv4")
	})
}

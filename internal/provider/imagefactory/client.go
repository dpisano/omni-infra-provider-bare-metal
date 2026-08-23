// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package imagefactory

import (
	"context"
	"fmt"
	"time"

	"github.com/siderolabs/image-factory/pkg/client"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"go.uber.org/zap"
)

// archArm64 is the arm64 value the iPXE handler passes down, matching iPXE's ${buildarch}.
const archArm64 = "arm64"

var agentModeExtensions = []string{
	// include all firmware extensions
	"siderolabs/amd-ucode",
	"siderolabs/amdgpu-firmware",
	"siderolabs/bnx2-bnx2x",
	"siderolabs/chelsio-firmware",
	"siderolabs/i915-ucode",
	"siderolabs/intel-ice-firmware",
	"siderolabs/intel-ucode",
	"siderolabs/qlogic-firmware",
	"siderolabs/realtek-firmware",
	// include the agent extension itself
	"siderolabs/metal-agent",
}

// agentModeExtensionsArm64 is the agent-mode extension set for arm64 machines.
//
// Every firmware extension in the amd64 set above is for hardware that does not appear on an arm64
// board: Intel and AMD CPU microcode, Intel integrated graphics microcode, AMD GPU firmware, and
// the firmware for server network cards. The factory publishes arm64 variants of all of them, but
// those either carry the x86 payloads verbatim or are near empty shells, so on arm64 they are
// transferred on every netboot for nothing. The CPU microcode alone is about 17 MB, which the
// factory prepends to the initramfs as an uncompressed early cpio archive.
//
// Realtek firmware is kept because USB Ethernet adapters on those chipsets are a common way to give
// a board a second network interface, and agent mode is useless to a machine that cannot reach the
// network.
//
// Note that this is deliberately narrower than what an arm64 server might need: an Ampere class
// machine with a Chelsio or QLogic card would not get its network firmware here.
var agentModeExtensionsArm64 = []string{
	"siderolabs/realtek-firmware",
	// include the agent extension itself
	"siderolabs/metal-agent",
}

// agentModeExtensionsForArch returns the agent-mode extension set to request for the given architecture.
func agentModeExtensionsForArch(arch string) []string {
	if arch == archArm64 {
		return agentModeExtensionsArm64
	}

	return agentModeExtensions
}

// Client is an image factory client.
type Client struct {
	factoryClient         *client.Client
	logger                *zap.Logger
	pxeBaseURL            string
	agentModeTalosVersion string
	secureBootEnabled     bool
}

// NewClient creates a new image factory client.
func NewClient(baseURL, pxeBaseURL, agentModeTalosVersion string, secureBootEnabled bool, logger *zap.Logger) (*Client, error) {
	factoryClient, err := client.New(baseURL)
	if err != nil {
		return nil, err
	}

	return &Client{
		pxeBaseURL:            pxeBaseURL,
		agentModeTalosVersion: agentModeTalosVersion,
		factoryClient:         factoryClient,
		secureBootEnabled:     secureBootEnabled,
		logger:                logger,
	}, nil
}

// SchematicIPXEURL ensures a schematic exists on the image factory and returns the iPXE URL to it.
//
// If agentMode is true, the schematic will be created with the firmware extensions and the metal-agent extension.
func (c *Client) SchematicIPXEURL(ctx context.Context, agentMode bool, talosVersion, arch string, extensions, extraKernelArgs []string) (string, error) {
	logger := c.logger.With(zap.String("talos_version", talosVersion), zap.String("arch", arch),
		zap.Strings("extensions", extensions), zap.Strings("extra_kernel_args", extraKernelArgs))

	logger.Debug("generate schematic iPXE URL")

	var metaValues []schematic.MetaValue

	if !agentMode && talosVersion == "" {
		return "", fmt.Errorf("talosVersion is required when not booting into agent mode")
	}

	if agentMode {
		talosVersion = c.agentModeTalosVersion

		extensions = agentModeExtensionsForArch(arch)
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	sch := schematic.Schematic{
		Customization: schematic.Customization{
			ExtraKernelArgs: extraKernelArgs,
			Meta:            metaValues,
			SystemExtensions: schematic.SystemExtensions{
				OfficialExtensions: extensions,
			},
		},
	}

	marshaled, err := sch.Marshal()
	if err != nil {
		return "", fmt.Errorf("failed to marshal schematic: %w", err)
	}

	logger.Debug("generated schematic", zap.String("schematic", string(marshaled)))

	schematicID, _, err := c.factoryClient.SchematicCreate(ctx, sch)
	if err != nil {
		return "", fmt.Errorf("failed to create schematic: %w", err)
	}

	ipxeURL := fmt.Sprintf("%s/pxe/%s/%s/metal-%s", c.pxeBaseURL, schematicID, talosVersion, arch)
	if c.secureBootEnabled {
		ipxeURL += "-secureboot"
	}

	logger.Debug("generated schematic iPXE URL", zap.String("ipxe_url", ipxeURL))

	return ipxeURL, nil
}

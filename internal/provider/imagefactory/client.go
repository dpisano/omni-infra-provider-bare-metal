// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package imagefactory

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/siderolabs/image-factory/pkg/client"
	"github.com/siderolabs/image-factory/pkg/schematic"
	"go.uber.org/zap"
)

// archArm64 is the arm64 value the iPXE handler passes down, matching iPXE's ${buildarch}.
const archArm64 = "arm64"

// x86MicrocodeExtensions carry microcode that only x86 hardware can load: CPU microcode for Intel
// and AMD processors, and the GuC and HuC microcode for Intel integrated graphics, which only ever
// accompanies an Intel x86 CPU.
//
// The factory does publish arm64 variants of them, but those still carry the x86 payloads, which an
// ARM kernel ignores outright. They are dropped on arm64 so a slow or bandwidth-constrained machine
// does not transfer them on every netboot.
//
// Only microcode is dropped. The remaining firmware extensions are for network and graphics
// hardware that an arm64 machine can genuinely have, so removing those could stop a machine from
// reaching the network in agent mode.
var x86MicrocodeExtensions = []string{
	"siderolabs/amd-ucode",
	"siderolabs/intel-ucode",
	"siderolabs/i915-ucode",
}

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

// agentModeExtensionsForArch returns the agent-mode extension set to request for the given architecture.
func agentModeExtensionsForArch(arch string) []string {
	if arch != archArm64 {
		return agentModeExtensions
	}

	return slices.DeleteFunc(slices.Clone(agentModeExtensions), func(extension string) bool {
		return slices.Contains(x86MicrocodeExtensions, extension)
	})
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

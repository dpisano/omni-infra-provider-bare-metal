// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package imagefactory_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/imagefactory"
)

func TestAgentModeExtensionsForArch(t *testing.T) {
	t.Parallel()

	t.Run("amd64 gets every extension, firmware and microcode included", func(t *testing.T) {
		t.Parallel()

		extensions := imagefactory.AgentModeExtensionsForArch("amd64")

		assert.Equal(t, imagefactory.AgentModeExtensions, extensions)
		assert.Contains(t, extensions, "siderolabs/intel-ucode")
		assert.Contains(t, extensions, "siderolabs/amd-ucode")
		assert.Contains(t, extensions, "siderolabs/chelsio-firmware")
	})

	t.Run("an unknown architecture falls back to the full set", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, imagefactory.AgentModeExtensions, imagefactory.AgentModeExtensionsForArch(""))
	})

	t.Run("arm64 carries no x86 hardware firmware at all", func(t *testing.T) {
		t.Parallel()

		extensions := imagefactory.AgentModeExtensionsForArch("arm64")

		assert.Equal(t, imagefactory.AgentModeExtensionsArm64, extensions)

		for _, dropped := range []string{
			"siderolabs/amd-ucode",
			"siderolabs/intel-ucode",
			"siderolabs/i915-ucode",
			"siderolabs/amdgpu-firmware",
			"siderolabs/bnx2-bnx2x",
			"siderolabs/chelsio-firmware",
			"siderolabs/intel-ice-firmware",
			"siderolabs/qlogic-firmware",
		} {
			assert.NotContains(t, extensions, dropped)
		}
	})

	t.Run("arm64 keeps the agent itself and the firmware a board can actually use", func(t *testing.T) {
		t.Parallel()

		extensions := imagefactory.AgentModeExtensionsForArch("arm64")

		// without the agent there is no agent mode at all
		assert.Contains(t, extensions, "siderolabs/metal-agent")

		// USB Ethernet adapters on Realtek chipsets are a common way to give a board a second interface
		assert.Contains(t, extensions, "siderolabs/realtek-firmware")
	})
}

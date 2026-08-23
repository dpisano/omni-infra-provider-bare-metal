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

	t.Run("amd64 gets every extension, microcode included", func(t *testing.T) {
		t.Parallel()

		extensions := imagefactory.AgentModeExtensionsForArch("amd64")

		assert.Equal(t, imagefactory.AgentModeExtensions, extensions)
		assert.Contains(t, extensions, "siderolabs/intel-ucode")
		assert.Contains(t, extensions, "siderolabs/amd-ucode")
	})

	t.Run("arm64 drops the x86 microcode and keeps everything else", func(t *testing.T) {
		t.Parallel()

		extensions := imagefactory.AgentModeExtensionsForArch("arm64")

		assert.NotContains(t, extensions, "siderolabs/intel-ucode")
		assert.NotContains(t, extensions, "siderolabs/amd-ucode")

		// the agent itself is the whole point, and the network firmware an arm64 server may need stays
		assert.Contains(t, extensions, "siderolabs/metal-agent")
		assert.Contains(t, extensions, "siderolabs/chelsio-firmware")
		assert.Contains(t, extensions, "siderolabs/qlogic-firmware")
		assert.Contains(t, extensions, "siderolabs/bnx2-bnx2x")
		assert.Contains(t, extensions, "siderolabs/intel-ice-firmware")
		assert.Contains(t, extensions, "siderolabs/realtek-firmware")

		assert.Len(t, extensions, len(imagefactory.AgentModeExtensions)-len(imagefactory.X86MicrocodeExtensions))
	})

	t.Run("the shared list is not mutated", func(t *testing.T) {
		t.Parallel()

		before := len(imagefactory.AgentModeExtensions)

		imagefactory.AgentModeExtensionsForArch("arm64")

		assert.Len(t, imagefactory.AgentModeExtensions, before)
		assert.Contains(t, imagefactory.AgentModeExtensions, "siderolabs/intel-ucode")
	})
}

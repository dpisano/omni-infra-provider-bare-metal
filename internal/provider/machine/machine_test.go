// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package machine_test

import (
	"testing"

	"github.com/cosi-project/runtime/pkg/resource"
	omnispecs "github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"

	"github.com/siderolabs/omni-infra-provider-bare-metal/api/specs"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/machine"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

const (
	testMachineID = "test-machine"
	testWipeID    = "test-wipe-id"
)

// machineFixture builds a machine that is accepted, allocated, has a BMC and needs no wipe,
// which is the state in which the installed-versus-not distinction actually decides the boot mode.
type machineFixture struct {
	infraMachine     *infra.Machine
	bmcConfiguration *resources.BMCConfiguration
	wipeStatus       *resources.WipeStatus
}

func newMachineFixture(installed bool) machineFixture {
	infraMachine := infra.NewMachine(testMachineID)
	infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_ACCEPTED
	infraMachine.TypedSpec().Value.ClusterTalosVersion = "v1.13.7"
	infraMachine.TypedSpec().Value.WipeId = testWipeID

	wipeStatus := resources.NewWipeStatus(testMachineID)
	wipeStatus.TypedSpec().Value.InitialWipeDone = true
	wipeStatus.TypedSpec().Value.LastWipeId = testWipeID

	if installed {
		// Omni saw an install event after the last wipe
		infraMachine.TypedSpec().Value.InstallEventId = 1
	}

	bmcConfiguration := resources.NewBMCConfiguration(testMachineID)
	bmcConfiguration.TypedSpec().Value.Manual = &specs.BMCConfigurationSpec_Manual{}

	return machineFixture{
		infraMachine:     infraMachine,
		bmcConfiguration: bmcConfiguration,
		wipeStatus:       wipeStatus,
	}
}

func TestRequiredBootModeAlwaysNetboot(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		mutate        func(machineFixture)
		name          string
		expected      machine.BootMode
		alwaysNetboot bool
		installed     bool
	}{
		{
			name:      "installed machine boots from disk",
			installed: true,
			expected:  machine.BootModeTalosDisk,
		},
		{
			name:          "installed machine keeps netbooting under always-netboot",
			installed:     true,
			alwaysNetboot: true,
			expected:      machine.BootModeTalosPXE,
		},
		{
			name:     "machine to be installed PXE boots Talos",
			expected: machine.BootModeTalosPXE,
		},
		{
			name:          "machine to be installed is unaffected by always-netboot",
			alwaysNetboot: true,
			expected:      machine.BootModeTalosPXE,
		},
		{
			// a rejected machine is left alone, so it is handed off to its disk either way
			name:      "rejected machine boots from disk even under always-netboot",
			installed: true,
			mutate: func(f machineFixture) {
				f.infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_REJECTED
			},
			alwaysNetboot: true,
			expected:      machine.BootModeTalosDisk,
		},
		{
			name:      "unallocated machine boots agent mode even under always-netboot",
			installed: true,
			mutate: func(f machineFixture) {
				f.infraMachine.TypedSpec().Value.ClusterTalosVersion = ""
			},
			alwaysNetboot: true,
			expected:      machine.BootModeAgentPXE,
		},
		{
			name:      "machine awaiting a wipe boots agent mode even under always-netboot",
			installed: true,
			mutate: func(f machineFixture) {
				f.wipeStatus.TypedSpec().Value.LastWipeId = "some-older-wipe-id"
			},
			alwaysNetboot: true,
			expected:      machine.BootModeAgentPXE,
		},
		{
			name:      "tearing down machine boots agent mode even under always-netboot",
			installed: true,
			mutate: func(f machineFixture) {
				f.infraMachine.Metadata().SetPhase(resource.PhaseTearingDown)
			},
			alwaysNetboot: true,
			expected:      machine.BootModeAgentPXE,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fixture := newMachineFixture(test.installed)

			if test.mutate != nil {
				test.mutate(fixture)
			}

			bootMode := machine.RequiredBootMode(
				fixture.infraMachine,
				fixture.bmcConfiguration,
				fixture.wipeStatus,
				machine.BootOptions{AlwaysNetboot: test.alwaysNetboot},
				zaptest.NewLogger(t),
			)

			assert.Equal(t, test.expected, bootMode)
		})
	}
}

// TestRequiredBootModeWithoutBMCConfig asserts a machine with no BMC configuration at all stays in
// agent mode, which is what keeps an undiscovered machine from ever being handed to a cluster.
func TestRequiredBootModeWithoutBMCConfig(t *testing.T) {
	t.Parallel()

	fixture := newMachineFixture(true)

	bootMode := machine.RequiredBootMode(fixture.infraMachine, nil, fixture.wipeStatus, machine.BootOptions{}, zaptest.NewLogger(t))

	assert.Equal(t, machine.BootModeAgentPXE, bootMode)
}

func TestIsManuallyPowered(t *testing.T) {
	t.Parallel()

	assert.False(t, machine.IsManuallyPowered(nil), "a missing configuration is not a manual one")

	withIPMI := resources.NewBMCConfiguration(testMachineID)
	withIPMI.TypedSpec().Value.Ipmi = &specs.BMCConfigurationSpec_IPMI{Address: "1.2.3.4"}

	assert.False(t, machine.IsManuallyPowered(withIPMI))

	manual := resources.NewBMCConfiguration(testMachineID)
	manual.TypedSpec().Value.Manual = &specs.BMCConfigurationSpec_Manual{}

	assert.True(t, machine.IsManuallyPowered(manual))
}

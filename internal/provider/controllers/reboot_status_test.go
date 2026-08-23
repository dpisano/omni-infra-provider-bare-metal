// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers_test

import (
	"context"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/controller/runtime"
	"github.com/cosi-project/runtime/pkg/state"
	omnispecs "github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-bare-metal/api/specs"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/pxe"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/controllers"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/machine"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

func TestNoRebootWhenPoweredOnNotRequired(t *testing.T) {
	t.Parallel()

	rebootCh := make(chan struct{}, 8)
	setPXEBootOnceCh := make(chan pxe.BootMode, 8)

	bmcClientFactory := &bmcClientFactoryMock{
		bmcClient: &bmcClientMock{
			poweredOn: true,
			rebootCh:  rebootCh,

			setPXEBootOnceCh: setPXEBootOnceCh,
		},
	}

	asserter := reconcileAsserter{t: t}

	withRuntime(
		t,
		func(_ context.Context, _ state.State, rt *runtime.Runtime, _ *zap.Logger) {
			controller := controllers.NewRebootStatusController(bmcClientFactory, &agentClientMock{}, 5*time.Minute, pxe.BootModeUEFI, controllers.RebootStatusControllerOptions{
				PostTransformFunc: asserter.incrementReconcile,
			})

			require.NoError(t, rt.RegisterQController(controller))
		},
		func(ctx context.Context, st state.State, _ *runtime.Runtime, _ *zap.Logger) {
			bmcConfiguration := resources.NewBMCConfiguration("test-machine")

			require.NoError(t, st.Create(ctx, bmcConfiguration))

			infraMachine := infra.NewMachine("test-machine")
			infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_ACCEPTED
			infraMachine.TypedSpec().Value.WipeId = "test-wipe-id"

			require.NoError(t, st.Create(ctx, infraMachine))

			machineStatus := resources.NewMachineStatus(infraMachine.Metadata().ID())
			machineStatus.TypedSpec().Value.PowerState = specs.PowerState_POWER_STATE_ON
			machineStatus.TypedSpec().Value.Initialized = true
			machineStatus.TypedSpec().Value.AgentAccessible = false

			require.NoError(t, st.Create(ctx, machineStatus))

			// The machine meets the following conditions:
			// - It is powered on, accepted, initialized, but the agent is not accessible -> mode mismatch.
			// - Its initial wipe is not done, so it requires a wipe.
			//   Because it requires a wipe, it needs to stay powered on.
			// -> The controller is expected to attempt to reboot into the agent mode.

			require.True(t, machine.RequiresPowerOn(infraMachine, nil), "machine should require power on")

			requireChReceive(ctx, t, setPXEBootOnceCh)
			requireChReceive(ctx, t, rebootCh)

			// Mark the machine as "does not need wiping": its initial wipe is done, and its last wipe ID matches the currently expected wipe ID.

			wipeStatus := resources.NewWipeStatus(infraMachine.Metadata().ID())
			wipeStatus.TypedSpec().Value.InitialWipeDone = true
			wipeStatus.TypedSpec().Value.LastWipeId = infraMachine.TypedSpec().Value.WipeId

			// ensure that we had at least one reconciliation
			numReconcilesBefore := asserter.requireNotZero(ctx)

			require.NoError(t, st.Create(ctx, wipeStatus))

			// At this point, the machine is not needed to be powered on,
			// as it is neither allocated, nor installed, nor requires a wipe.
			require.False(t, machine.RequiresPowerOn(infraMachine, wipeStatus), "machine should not require power on")

			// wait for a new reconciliation to happen
			asserter.requireReconcile(ctx, numReconcilesBefore)

			require.Empty(t, rebootCh, "reboot channel should be empty, no reboot should be issued")
			require.Empty(t, setPXEBootOnceCh, "setPXEBootOnce channel should be empty, no PXE boot mode should be set")
		},
	)
}

// TestRebootMachineWithoutBMC asserts that a machine with no BMC is rebooted over its agent,
// and that the provider gives up quietly when the agent is not reachable either.
func TestRebootMachineWithoutBMC(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		agentAccessible bool
	}{
		{name: "agent-accessible", agentAccessible: true},
		{name: "agent-unreachable", agentAccessible: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			bmcRebootCh := make(chan struct{}, 8)
			setPXEBootOnceCh := make(chan pxe.BootMode, 8)
			agentRebootCh := make(chan string, 8)

			bmcClientFactory := &bmcClientFactoryMock{
				bmcClient: &bmcClientMock{
					rebootCh:         bmcRebootCh,
					setPXEBootOnceCh: setPXEBootOnceCh,

					// a machine without a BMC supports nothing out of band
					capabilities: &bmc.Capabilities{},
				},
			}

			agentClient := &agentClientMock{rebootCh: agentRebootCh}
			asserter := reconcileAsserter{t: t}

			withRuntime(
				t,
				func(_ context.Context, _ state.State, rt *runtime.Runtime, _ *zap.Logger) {
					controller := controllers.NewRebootStatusController(bmcClientFactory, agentClient, 0, pxe.BootModeUEFI, controllers.RebootStatusControllerOptions{
						PostTransformFunc: asserter.incrementReconcile,
					})

					require.NoError(t, rt.RegisterQController(controller))
				},
				func(ctx context.Context, st state.State, _ *runtime.Runtime, _ *zap.Logger) {
					bmcConfiguration := resources.NewBMCConfiguration("test-machine")
					bmcConfiguration.TypedSpec().Value.Manual = &specs.BMCConfigurationSpec_Manual{}

					require.NoError(t, st.Create(ctx, bmcConfiguration))

					infraMachine := infra.NewMachine("test-machine")
					infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_ACCEPTED
					infraMachine.TypedSpec().Value.WipeId = "test-wipe-id"

					require.NoError(t, st.Create(ctx, infraMachine))

					machineStatus := resources.NewMachineStatus(infraMachine.Metadata().ID())
					machineStatus.TypedSpec().Value.Initialized = true
					machineStatus.TypedSpec().Value.AgentAccessible = test.agentAccessible

					require.NoError(t, st.Create(ctx, machineStatus))

					if test.agentAccessible {
						// The machine is allocated and no longer needs a wipe, so it must leave agent mode
						// and PXE boot Talos. Its agent is up, which is the only reboot channel it has.
						infraMachine.TypedSpec().Value.ClusterTalosVersion = "v1.13.7"

						require.NoError(t, st.Update(ctx, infraMachine))

						wipeStatus := resources.NewWipeStatus(infraMachine.Metadata().ID())
						wipeStatus.TypedSpec().Value.InitialWipeDone = true
						wipeStatus.TypedSpec().Value.LastWipeId = infraMachine.TypedSpec().Value.WipeId

						require.NoError(t, st.Create(ctx, wipeStatus))

						require.Equal(t, infraMachine.Metadata().ID(), requireChReceive(ctx, t, agentRebootCh))
					} else {
						// The machine still requires its initial wipe, so it must reach agent mode, but its
						// agent is not up and it has no BMC: nothing the provider can do. The controller is
						// expected to reconcile, decide it cannot act, and stop rather than retry in a loop.
						asserter.requireNotZero(ctx)

						require.Empty(t, agentRebootCh, "no reboot should be issued over the agent")
					}

					require.Empty(t, bmcRebootCh, "no reboot should be issued over the BMC")
					require.Empty(t, setPXEBootOnceCh, "a machine without a BMC cannot be told to PXE boot once")
				},
			)
		})
	}
}

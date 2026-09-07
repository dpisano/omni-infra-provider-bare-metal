// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers_test

import (
	"context"
	"testing"

	"github.com/cosi-project/runtime/pkg/controller/runtime"
	"github.com/cosi-project/runtime/pkg/resource/rtestutils"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/containers"
	"github.com/siderolabs/gen/pair"
	omnispecs "github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	agentpb "github.com/siderolabs/talos-metal-agent/api/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/controllers"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

func TestBMCConfiguration(t *testing.T) {
	t.Parallel()

	var getPowerMgmtResponseMap containers.ConcurrentMap[string, *agentpb.GetPowerManagementResponse]

	getPowerMgmtResponseMap.Set("test-machine", &agentpb.GetPowerManagementResponse{
		Ipmi: &agentpb.GetPowerManagementResponse_IPMI{
			Address: "5.6.7.8",
			Port:    5678,
		},
	})

	setPowerMgmtRequestCh := make(chan pair.Pair[string, *agentpb.SetPowerManagementRequest])

	agentClient := &agentClientMock{
		getPowerMgmtResponseMap: &getPowerMgmtResponseMap,
		setPowerMgmtRequestCh:   setPowerMgmtRequestCh,
	}

	withRuntime(
		t,
		func(_ context.Context, _ state.State, rt *runtime.Runtime, _ *zap.Logger) {
			controller := controllers.NewBMCConfigurationController(agentClient, nil, controllers.BMCConfigurationControllerOptions{})
			require.NoError(t, rt.RegisterQController(controller))
		},

		func(ctx context.Context, st state.State, _ *runtime.Runtime, _ *zap.Logger) {
			machineStatus := resources.NewMachineStatus("test-machine")

			machineStatus.TypedSpec().Value.AgentAccessible = true

			require.NoError(t, st.Create(ctx, machineStatus))

			// create a user-provided BMC config and assert that it is stored

			bmcConfig := infra.NewBMCConfig("test-machine")

			bmcConfig.TypedSpec().Value.Config = &omnispecs.InfraMachineBMCConfigSpec{
				Ipmi: &omnispecs.InfraMachineBMCConfigSpec_IPMI{
					Address:  "1.2.3.4",
					Port:     1234,
					Username: "test-user",
					Password: "test-password",
				},
			}

			require.NoError(t, st.Create(ctx, bmcConfig))

			// create the infra machine to trigger the controller

			infraMachine := infra.NewMachine("test-machine")

			infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_ACCEPTED

			require.NoError(t, st.Create(ctx, infraMachine))

			rtestutils.AssertResource(ctx, t, st, infraMachine.Metadata().ID(), func(res *resources.BMCConfiguration, assertion *assert.Assertions) {
				assertion.True(res.TypedSpec().Value.ManuallyConfigured)

				assertion.Equal("1.2.3.4", res.TypedSpec().Value.Ipmi.Address)
				assertion.Equal(uint32(1234), res.TypedSpec().Value.Ipmi.Port)
				assertion.Equal("test-user", res.TypedSpec().Value.Ipmi.Username)
				assertion.Equal("test-password", res.TypedSpec().Value.Ipmi.Password)
			})

			// remove user-provided config, so we will go back to the config over the agent

			rtestutils.Destroy[*infra.BMCConfig](ctx, t, st, []string{bmcConfig.Metadata().ID()})

			setPowerMgmtRequest := requireChReceive(ctx, t, setPowerMgmtRequestCh)

			assert.Equal(t, "test-machine", setPowerMgmtRequest.F1)
			assert.Equal(t, controllers.IPMIUsername, setPowerMgmtRequest.F2.Ipmi.Username)
			assert.NotEqual(t, "test-password", setPowerMgmtRequest.F2.Ipmi.Password)
			assert.Len(t, setPowerMgmtRequest.F2.Ipmi.Password, controllers.IPMIPasswordLength)

			rtestutils.AssertResource(ctx, t, st, infraMachine.Metadata().ID(), func(res *resources.BMCConfiguration, assertion *assert.Assertions) {
				assertion.False(res.TypedSpec().Value.ManuallyConfigured)

				assertion.Equal("5.6.7.8", res.TypedSpec().Value.Ipmi.Address)
				assertion.Equal(uint32(5678), res.TypedSpec().Value.Ipmi.Port)
				assertion.Equal(controllers.IPMIUsername, res.TypedSpec().Value.Ipmi.Username)
				assertion.Equal(setPowerMgmtRequest.F2.Ipmi.Password, res.TypedSpec().Value.Ipmi.Password)

				// the machine has a real BMC, so it is not manually powered
				assertion.Nil(res.TypedSpec().Value.Manual)
			})
		},
	)
}

// TestBMCConfigurationWithoutBMC covers a machine that has no BMC at all, in both the ways one can
// present itself to the provider.
//
// The error case is the real one. The agent has no way to say "there is nothing here": outside test
// mode GetPowerManagement only ever returns with the IPMI field filled in, so a machine with no BMC
// surfaces as the agent failing to open the local IPMI device. The empty response is accepted too,
// but no released agent sends it.
func TestBMCConfigurationWithoutBMC(t *testing.T) {
	t.Parallel()

	const noIPMIDevice = "error creating ipmi client: failed to create IPMI client: ipmi dev file not opened"

	for _, test := range []struct {
		response                *agentpb.GetPowerManagementResponse
		err                     error
		name                    string
		allowMachinesWithoutBMC bool
		wantManual              bool
	}{
		{
			name:                    "no ipmi device, allowed",
			err:                     status.Error(codes.Internal, noIPMIDevice),
			allowMachinesWithoutBMC: true,
			wantManual:              true,
		},
		{
			name:                    "no ipmi device, not allowed",
			err:                     status.Error(codes.Internal, noIPMIDevice),
			allowMachinesWithoutBMC: false,
		},
		{
			// the client was created, so there is a BMC and something else went wrong: retry rather
			// than writing the machine off as having none, which would stick
			name:                    "a BMC that answered badly is not treated as absent",
			err:                     status.Error(codes.Internal, "error getting bmc ip port: context deadline exceeded"),
			allowMachinesWithoutBMC: true,
		},
		{
			name:                    "an unrelated failure is not treated as absent",
			err:                     status.Error(codes.Unavailable, "connection closed"),
			allowMachinesWithoutBMC: true,
		},
		{
			// neither IPMI nor API: the machine has no BMC
			name:                    "an empty response, allowed",
			response:                &agentpb.GetPowerManagementResponse{},
			allowMachinesWithoutBMC: true,
			wantManual:              true,
		},
		{
			name:                    "an empty response, not allowed",
			response:                &agentpb.GetPowerManagementResponse{},
			allowMachinesWithoutBMC: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var getPowerMgmtResponseMap containers.ConcurrentMap[string, *agentpb.GetPowerManagementResponse]

			if test.response != nil {
				getPowerMgmtResponseMap.Set("test-machine", test.response)
			}

			agentClient := &agentClientMock{
				getPowerMgmtResponseMap: &getPowerMgmtResponseMap,
				getPowerMgmtErr:         test.err,
			}

			withRuntime(
				t,
				func(_ context.Context, _ state.State, rt *runtime.Runtime, _ *zap.Logger) {
					controller := controllers.NewBMCConfigurationController(agentClient, nil, controllers.BMCConfigurationControllerOptions{
						AllowMachinesWithoutBMC: test.allowMachinesWithoutBMC,
					})

					require.NoError(t, rt.RegisterQController(controller))
				},

				func(ctx context.Context, st state.State, _ *runtime.Runtime, _ *zap.Logger) {
					machineStatus := resources.NewMachineStatus("test-machine")

					machineStatus.TypedSpec().Value.AgentAccessible = true

					require.NoError(t, st.Create(ctx, machineStatus))

					infraMachine := infra.NewMachine("test-machine")

					infraMachine.TypedSpec().Value.AcceptanceStatus = omnispecs.InfraMachineConfigSpec_ACCEPTED

					require.NoError(t, st.Create(ctx, infraMachine))

					if !test.wantManual {
						// the controller keeps failing, so no BMC configuration is ever produced
						rtestutils.AssertNoResource[*resources.BMCConfiguration](ctx, t, st, infraMachine.Metadata().ID())

						return
					}

					rtestutils.AssertResource(ctx, t, st, infraMachine.Metadata().ID(), func(res *resources.BMCConfiguration, assertion *assert.Assertions) {
						assertion.NotNil(res.TypedSpec().Value.Manual)
						assertion.Nil(res.TypedSpec().Value.Ipmi)
						assertion.Nil(res.TypedSpec().Value.Api)
						assertion.False(res.TypedSpec().Value.ManuallyConfigured)
					})
				},
			)
		})
	}
}

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/controller/generic/qtransform"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/siderolabs/gen/xerrors"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	agentpb "github.com/siderolabs/talos-metal-agent/api/agent"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/siderolabs/omni-infra-provider-bare-metal/api/specs"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/meta"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

// BMCAPIAddressReader is the interface for reading power management information from the API state directory.
type BMCAPIAddressReader interface {
	ReadManagementAddress(id resource.ID, logger *zap.Logger) (string, error)
}

// BMCConfigurationController manages machine power management.
type BMCConfigurationController = qtransform.QController[*infra.Machine, *resources.BMCConfiguration]

// BMCConfigurationControllerOptions defines options for the BMCConfigurationController.
type BMCConfigurationControllerOptions struct {
	// AllowMachinesWithoutBMC makes a machine whose agent reports no power management at all
	// be recorded as manually powered instead of being rejected.
	AllowMachinesWithoutBMC bool
}

// NewBMCConfigurationController creates a new BMCConfigurationController.
func NewBMCConfigurationController(agentClient AgentClient, bmcAPIAddressReader BMCAPIAddressReader, options BMCConfigurationControllerOptions) *BMCConfigurationController {
	helper := &bmcConfigurationControllerHelper{
		agentClient:         agentClient,
		bmcAPIAddressReader: bmcAPIAddressReader,
		options:             options,
	}

	return qtransform.NewQController(
		qtransform.Settings[*infra.Machine, *resources.BMCConfiguration]{
			Name: meta.ProviderID.String() + ".BMCConfigurationController",
			MapMetadataFunc: func(infraMachine *infra.Machine) *resources.BMCConfiguration {
				return resources.NewBMCConfiguration(infraMachine.Metadata().ID())
			},
			UnmapMetadataFunc: func(bmcConfiguration *resources.BMCConfiguration) *infra.Machine {
				return infra.NewMachine(bmcConfiguration.Metadata().ID())
			},
			TransformFunc: helper.transform,
		},
		qtransform.WithConcurrency(4),
		qtransform.WithExtraMappedInput[*resources.MachineStatus](qtransform.MapperSameID[*infra.Machine]()),
		qtransform.WithExtraMappedInput[*infra.BMCConfig](qtransform.MapperSameID[*infra.Machine]()),
		qtransform.WithIgnoreTeardownUntil(), // keep this resource around until all other controllers are done with it
	)
}

type bmcConfigurationControllerHelper struct {
	agentClient         AgentClient
	bmcAPIAddressReader BMCAPIAddressReader
	options             BMCConfigurationControllerOptions
}

func (helper *bmcConfigurationControllerHelper) transform(ctx context.Context, r controller.Reader, logger *zap.Logger,
	infraMachine *infra.Machine, bmcConfiguration *resources.BMCConfiguration,
) error {
	machineStatus, err := safe.ReaderGetByID[*resources.MachineStatus](ctx, r, infraMachine.Metadata().ID())
	if err != nil && !state.IsNotFoundError(err) {
		return err
	}

	bmcConfig, err := safe.ReaderGetByID[*infra.BMCConfig](ctx, r, infraMachine.Metadata().ID())
	if err != nil && !state.IsNotFoundError(err) {
		return err
	}

	if err = validateInfraMachine(infraMachine, logger); err != nil {
		return err
	}

	if machineStatus == nil {
		logger.Debug("machine status not found, skip")

		return xerrors.NewTaggedf[qtransform.SkipReconcileTag]("machine status not found")
	}

	if !machineStatus.TypedSpec().Value.AgentAccessible {
		logger.Info("agent is not accessible, skip")

		return xerrors.NewTaggedf[qtransform.SkipReconcileTag]("agent is not accessible")
	}

	id := infraMachine.Metadata().ID()

	if bmcConfig != nil {
		return helper.storeUserProvidedBMCConfig(bmcConfig, bmcConfiguration, logger)
	}

	alreadyInitialized := !bmcConfiguration.TypedSpec().Value.ManuallyConfigured &&
		(bmcConfiguration.TypedSpec().Value.Api != nil ||
			bmcConfiguration.TypedSpec().Value.Ipmi != nil ||
			bmcConfiguration.TypedSpec().Value.Manual != nil)

	if alreadyInitialized {
		logger.Debug("bmc config already initialized, skip")

		return xerrors.NewTaggedf[qtransform.SkipReconcileTag]("bmc config already initialized")
	}

	powerManagementOnAgent, err := helper.agentClient.GetPowerManagement(ctx, id)
	if err != nil {
		if !helper.hasNoBMC(nil, err) {
			return fmt.Errorf("failed to get power management information: %w", err)
		}

		logger.Info("machine has no BMC to talk to, record it as manually powered", zap.Error(err))

		bmcConfiguration.TypedSpec().Value.ManuallyConfigured = false
		bmcConfiguration.TypedSpec().Value.Manual = &specs.BMCConfigurationSpec_Manual{}

		return nil
	}

	if helper.hasNoBMC(powerManagementOnAgent, nil) {
		logger.Info("machine reports no power management, record it as manually powered")

		bmcConfiguration.TypedSpec().Value.ManuallyConfigured = false
		bmcConfiguration.TypedSpec().Value.Manual = &specs.BMCConfigurationSpec_Manual{}

		return nil
	}

	ipmiPassword, err := helper.ensurePowerManagementOnAgent(ctx, id, powerManagementOnAgent)
	if err != nil {
		return fmt.Errorf("failed to ensure power management on agent: %w", err)
	}

	bmcConfiguration.TypedSpec().Value.ManuallyConfigured = false

	if powerManagementOnAgent.Api != nil {
		address, addressErr := helper.bmcAPIAddressReader.ReadManagementAddress(id, logger)
		if addressErr != nil {
			return addressErr
		}

		bmcConfiguration.TypedSpec().Value.Api = &specs.BMCConfigurationSpec_API{
			Address: address,
		}

		logger.Debug("api bmc config initialized", zap.String("api_address", address))
	}

	if powerManagementOnAgent.Ipmi != nil {
		bmcConfiguration.TypedSpec().Value.Ipmi = &specs.BMCConfigurationSpec_IPMI{
			Address:  powerManagementOnAgent.Ipmi.Address,
			Port:     powerManagementOnAgent.Ipmi.Port,
			Username: IPMIUsername,
			Password: ipmiPassword,
		}

		logger.Debug("ipmi bmc config initialized", zap.String("ipmi_address", powerManagementOnAgent.Ipmi.Address), zap.String("ipmi_username", IPMIUsername))
	}

	return nil
}

// hasNoBMC reports whether the machine has no BMC at all and the provider is allowed to accept it.
//
// Such a machine is recorded as manually powered so it can still become ready to use, rather than
// failing here on every reconcile forever. Exactly one of powerManagement and err is expected to be
// set, matching whichever way GetPowerManagement returned.
//
// The error case is the one that happens. The agent cannot say "there is nothing here": outside
// test mode GetPowerManagement only ever returns with the IPMI field filled in, so the sole way it
// reports a machine with no BMC is by failing to open the local IPMI device and turning that into
// an Internal error. Recognizing that is what makes AllowMachinesWithoutBMC work on real hardware
// at all; the empty response it also accepts is something no released agent sends.
func (helper *bmcConfigurationControllerHelper) hasNoBMC(powerManagement *agentpb.GetPowerManagementResponse, err error) bool {
	if !helper.options.AllowMachinesWithoutBMC {
		return false
	}

	if err != nil {
		return isNoBMCError(err)
	}

	return powerManagement.GetApi() == nil && powerManagement.GetIpmi() == nil
}

// isNoBMCError reports whether a GetPowerManagement failure means the machine has no BMC, rather
// than something that might succeed on the next attempt.
//
// Two things have to hold, and each rules out a different way of being wrong. Recording a machine
// as manually powered is sticky, since the result short-circuits every later reconcile, so this
// errs towards retrying.
//
// The status code must be Internal, which is what the agent returns for its own failures. Transport
// trouble between the provider and the agent arrives as Unavailable, DeadlineExceeded or Canceled,
// and a machine must never lose its BMC to a momentary blip.
//
// The message must mention IPMI, which is the subsystem that could not be reached. This is what
// keeps the agent's later steps out: failing to read the BMC's address, for one, is also Internal,
// but it means the machine does have a BMC that something else went wrong with.
//
// Matching a message at all is unpleasant, and it is only the subsystem name that is matched rather
// than any particular wording, so rewording the failure upstream does not silently stop this
// working. There is no better signal: nothing structured about "this machine has no BMC" crosses
// the gRPC boundary.
func isNoBMCError(err error) bool {
	if status.Code(err) != codes.Internal {
		return false
	}

	return strings.Contains(strings.ToLower(err.Error()), "ipmi")
}

func (helper *bmcConfigurationControllerHelper) storeUserProvidedBMCConfig(userConfig *infra.BMCConfig, bmcConfiguration *resources.BMCConfiguration, logger *zap.Logger) error {
	config := userConfig.TypedSpec().Value.Config
	if config == nil {
		return fmt.Errorf("user provided BMC config is nil")
	}

	logger.Info("initialize BMC config from user-provided config")

	bmcConfiguration.TypedSpec().Value.ManuallyConfigured = true

	// the operator handed us real BMC credentials, so the machine is no longer manually powered
	bmcConfiguration.TypedSpec().Value.Manual = nil

	if config.Ipmi != nil {
		port := config.Ipmi.Port
		if port == 0 {
			port = IPMIDefaultPort
		}

		bmcConfiguration.TypedSpec().Value.Ipmi = &specs.BMCConfigurationSpec_IPMI{
			Address:  config.Ipmi.Address,
			Port:     port,
			Username: config.Ipmi.Username,
			Password: config.Ipmi.Password,
		}

		logger.Info(
			"user-provided ipmi config initialized",
			zap.String("ipmi_address", config.Ipmi.Address),
			zap.String("ipmi_username", config.Ipmi.Username),
			zap.Uint32("ipmi_port", port),
		)
	}

	if config.Api != nil {
		bmcConfiguration.TypedSpec().Value.Api = &specs.BMCConfigurationSpec_API{
			Address: config.Api.Address,
		}

		logger.Info("user-provided api config initialized", zap.String("api_address", config.Api.Address))
	}

	return nil
}

// ensurePowerManagementOnAgent ensures that the power management (e.g., IPMI) is configured and credentials are set on the Talos machine running agent.
func (helper *bmcConfigurationControllerHelper) ensurePowerManagementOnAgent(ctx context.Context, id resource.ID,
	powerManagement *agentpb.GetPowerManagementResponse,
) (ipmiPassword string, err error) {
	if powerManagement.Api == nil && powerManagement.Ipmi == nil {
		return "", fmt.Errorf("machine did not provide any power management information: " +
			"if it genuinely has no BMC, start the provider with --allow-machines-without-bmc")
	}

	var (
		api  *agentpb.SetPowerManagementRequest_API
		ipmi *agentpb.SetPowerManagementRequest_IPMI
	)

	if powerManagement.Api != nil {
		api = &agentpb.SetPowerManagementRequest_API{}
	}

	if powerManagement.Ipmi != nil {
		ipmiPassword, err = generateIPMIPassword()
		if err != nil {
			return "", err
		}

		ipmi = &agentpb.SetPowerManagementRequest_IPMI{
			Username: IPMIUsername,
			Password: ipmiPassword,
		}
	}

	if err = helper.agentClient.SetPowerManagement(ctx, id, &agentpb.SetPowerManagementRequest{
		Api:  api,
		Ipmi: ipmi,
	}); err != nil {
		return "", err
	}

	return ipmiPassword, nil
}

var runes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

// generateIPMIPassword returns a random password of length 16 for IPMI.
func generateIPMIPassword() (string, error) {
	b := make([]rune, IPMIPasswordLength)
	for i := range b {
		rando, err := rand.Int(rand.Reader, big.NewInt(int64(len(runes))))
		if err != nil {
			return "", err
		}

		b[i] = runes[rando.Int64()]
	}

	return string(b), nil
}

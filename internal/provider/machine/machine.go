// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package machine provides utilities for determining the required state of a machine.
package machine

import (
	"github.com/cosi-project/runtime/pkg/resource"
	omnispecs "github.com/siderolabs/omni/client/api/omni/specs"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

// BootMode represents the boot mode of a machine.
type BootMode string

const (
	// BootModeAgentPXE is the boot mode for agent PXE boot.
	BootModeAgentPXE BootMode = "agent-pxe"
	// BootModeTalosPXE is the boot mode for Talos PXE boot.
	BootModeTalosPXE BootMode = "talos-pxe"
	// BootModeTalosDisk is the boot mode for Talos disk boot.
	BootModeTalosDisk BootMode = "talos-disk"
)

// IsManuallyPowered returns true if the machine has no BMC, so a human controls its power.
//
// The provider cannot read such a machine's power state, power it on or off, or set a one-time
// boot device, so it relies on the machine being configured to network boot first and on the agent
// for reboots.
func IsManuallyPowered(bmcConfiguration *resources.BMCConfiguration) bool {
	return bmcConfiguration != nil && bmcConfiguration.TypedSpec().Value.Manual != nil
}

// IsInstalled returns true if the machine is installed.
func IsInstalled(infraMachine *infra.Machine, wipeStatus *resources.WipeStatus) bool {
	if infraMachine == nil {
		return false
	}

	installEventID := infraMachine.TypedSpec().Value.InstallEventId
	lastWipeInstallEventID := uint64(0)

	if wipeStatus != nil {
		lastWipeInstallEventID = wipeStatus.TypedSpec().Value.LastWipeInstallEventId
	}

	return installEventID > lastWipeInstallEventID
}

// RequiresWipe returns true if the machine needs to be wiped.
func RequiresWipe(infraMachine *infra.Machine, wipeStatus *resources.WipeStatus) bool {
	// maybe check acceptance here (or here as well)
	if infraMachine == nil || wipeStatus == nil || !wipeStatus.TypedSpec().Value.InitialWipeDone {
		return true
	}

	return infraMachine.TypedSpec().Value.WipeId != wipeStatus.TypedSpec().Value.LastWipeId
}

// BootOptions tunes how the required boot mode is decided.
type BootOptions struct {
	// AlwaysNetboot keeps serving Talos over the network to an installed machine, instead of
	// handing it off to boot from its disk.
	//
	// It is meant for machines whose firmware cannot boot the installed system, such as a
	// Raspberry Pi, whose installed disk lacks the board's bootloader because Omni installs Talos
	// without a board overlay. The disk still holds the machine's state, only the kernel and
	// initramfs come from the provider, which also keeps the provider in control of every boot.
	//
	// The cost is that the provider becomes a hard dependency of every boot: while it is down, a
	// machine that reboots does not come back up.
	AlwaysNetboot bool
}

// RequiredBootMode returns the required boot mode for the machine.
func RequiredBootMode(infraMachine *infra.Machine, bmcConfiguration *resources.BMCConfiguration, wipeStatus *resources.WipeStatus,
	options BootOptions, logger *zap.Logger,
) BootMode {
	installed := IsInstalled(infraMachine, wipeStatus)
	requiresWipe := RequiresWipe(infraMachine, wipeStatus)
	acceptanceStatus := omnispecs.InfraMachineConfigSpec_PENDING
	infraMachineTearingDown := false
	allocated := false

	if infraMachine != nil {
		acceptanceStatus = infraMachine.TypedSpec().Value.AcceptanceStatus
		infraMachineTearingDown = infraMachine.Metadata().Phase() == resource.PhaseTearingDown
		allocated = infraMachine.TypedSpec().Value.ClusterTalosVersion != ""
	}

	acceptancePending := acceptanceStatus == omnispecs.InfraMachineConfigSpec_PENDING
	rejected := acceptanceStatus == omnispecs.InfraMachineConfigSpec_REJECTED
	requiresPowerMgmtConfig := bmcConfiguration == nil

	bootIntoAgentMode := infraMachineTearingDown || acceptancePending || !allocated || requiresPowerMgmtConfig || requiresWipe

	var requiredBootMode BootMode

	switch {
	case rejected:
		// a rejected machine is left alone, so it is handed off to its disk even under AlwaysNetboot
		requiredBootMode = BootModeTalosDisk
	case bootIntoAgentMode:
		requiredBootMode = BootModeAgentPXE
	case installed && !options.AlwaysNetboot:
		requiredBootMode = BootModeTalosDisk
	default:
		requiredBootMode = BootModeTalosPXE
	}

	logger.With(
		zap.Bool("infra_machine_tearing_down", infraMachineTearingDown),
		zap.Bool("requires_power_mgmt_config", requiresPowerMgmtConfig),
		zap.Bool("installed", installed),
		zap.Bool("always_netboot", options.AlwaysNetboot),
		zap.Stringer("acceptance_status", acceptanceStatus),
		zap.String("required_boot_mode", string(requiredBootMode)),
	).Debug("determined boot mode")

	return requiredBootMode
}

// RequiresPXEBoot returns true if the machine requires to be PXE booted.
func RequiresPXEBoot(requiredBootMode BootMode) bool {
	return requiredBootMode == BootModeAgentPXE || requiredBootMode == BootModeTalosPXE
}

// RequiresPowerOn returns true if the machine requires to be powered on.
func RequiresPowerOn(infraMachine *infra.Machine, wipeStatus *resources.WipeStatus) bool {
	allocated := infraMachine.TypedSpec().Value.ClusterTalosVersion != ""
	installed := IsInstalled(infraMachine, wipeStatus)
	requiresWipe := RequiresWipe(infraMachine, wipeStatus)

	return allocated || installed || requiresWipe
}

// IsPowerOffActive returns true if the power-off request on the InfraMachine is currently being honored by the provider.
// The request is considered active when the provider has acknowledged the current PowerOffRequestId and the machine
// has not gone through a deallocation cycle since (wipe_id matches the one captured at acknowledgment).
func IsPowerOffActive(infraMachine *infra.Machine, powerOperation *resources.PowerOperation) bool {
	if infraMachine == nil || powerOperation == nil {
		return false
	}

	opSpec := powerOperation.TypedSpec().Value

	return opSpec.LastPowerOffId != "" &&
		opSpec.LastPowerOffId == infraMachine.TypedSpec().Value.PowerOffRequestId &&
		opSpec.WipeIdAtPowerOff == infraMachine.TypedSpec().Value.WipeId
}

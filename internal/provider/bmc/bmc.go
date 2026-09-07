// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package bmc provides BMC functionality for machines.
package bmc

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-bare-metal/api/specs"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/api"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/ipmi"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/manual"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/pxe"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/redfish"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
)

// backend is the raw BMC transport, implemented by the ipmi, redfish, api and manual packages.
type backend interface {
	Close(ctx context.Context) error
	Reboot(ctx context.Context) error
	IsPoweredOn(ctx context.Context) (bool, error)
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	SetPXEBootOnce(ctx context.Context, mode pxe.BootMode) error
	ResetBootDevice(ctx context.Context) error
}

// Capabilities describes what a BMC client can actually do for a machine.
//
// A machine with a real BMC supports everything. A machine without one supports nothing, and
// callers must check here before issuing an operation rather than calling and handling the failure.
type Capabilities struct {
	// PowerState reports whether IsPoweredOn returns a meaningful answer.
	PowerState bool
	// PowerControl reports whether PowerOn and PowerOff are usable.
	PowerControl bool
	// BootDeviceControl reports whether SetPXEBootOnce and ResetBootDevice are usable.
	BootDeviceControl bool
	// Reboot reports whether Reboot is usable.
	Reboot bool
}

// FullCapabilities is what a machine with a working BMC supports.
func FullCapabilities() Capabilities {
	return Capabilities{PowerState: true, PowerControl: true, BootDeviceControl: true, Reboot: true}
}

// Client is the interface to interact with a single machine to send BMC commands to it.
type Client interface {
	backend

	// Capabilities reports which of the operations above are actually supported for this machine.
	Capabilities() Capabilities
}

// ClientFactory is a factory to create BMC clients.
type ClientFactory struct {
	redfishAvailabilityCache map[string]bool
	options                  ClientFactoryOptions
	redfishAvailabilityMu    sync.Mutex
}

// ClientFactoryOptions contains options for the client factory.
type ClientFactoryOptions struct {
	RedfishOptions redfish.Options
}

// NewClientFactory creates a new BMC client factory.
func NewClientFactory(options ClientFactoryOptions) *ClientFactory {
	return &ClientFactory{
		options:                  options,
		redfishAvailabilityCache: map[string]bool{},
	}
}

// GetClient returns a BMC client for the given bare metal machine.
func (factory *ClientFactory) GetClient(ctx context.Context, config *resources.BMCConfiguration, logger *zap.Logger) (Client, error) {
	if config == nil {
		return nil, fmt.Errorf("cannot get BMC client: config is nil")
	}

	spec := config.TypedSpec().Value

	if spec.Ipmi == nil && spec.Api == nil && spec.Manual == nil {
		return nil, fmt.Errorf("invalid BMC config: IPMI, API and manual fields are all nil")
	}

	// A manual machine has no BMC, so it takes precedence: there is nothing to talk to,
	// regardless of what else the config might carry.
	if spec.Manual != nil {
		return &loggingClient{
			client: manual.NewClient(),
			logger: logger.With(zap.String("bmc_client", "manual")),
		}, nil
	}

	if spec.Api != nil {
		apiClient, err := api.NewClient(spec.Api)
		if err != nil {
			return nil, err
		}

		return &loggingClient{client: apiClient, capabilities: FullCapabilities(), logger: logger.With(zap.String("bmc_client", "api"))}, nil
	}

	useRedfish := factory.options.RedfishOptions.UseAlways || (factory.options.RedfishOptions.UseWhenAvailable && factory.redfishAvailable(ctx, config.Metadata().ID(), spec.Ipmi, logger))

	if useRedfish {
		logger = logger.With(zap.String("bmc_client", "redfish"))
		redfishClient := redfish.NewClient(factory.options.RedfishOptions, spec.Ipmi.Address, spec.Ipmi.Username, spec.Ipmi.Password, logger)

		return &loggingClient{client: redfishClient, capabilities: FullCapabilities(), logger: logger}, nil
	}

	ipmiClient, err := ipmi.NewClient(ctx, spec.Ipmi)
	if err != nil {
		return nil, err
	}

	return &loggingClient{client: ipmiClient, capabilities: FullCapabilities(), logger: logger.With(zap.String("bmc_client", "ipmi"))}, nil
}

// redfishAvailable reports whether the machine's BMC speaks Redfish, caching the
// result. The cache uses the machine ID together with the address as its key.
// The machine ID keeps machines that share an address apart (e.g. emulated BMCs
// on distinct loopback ports, which a plain address would collapse into a single
// entry). The address is part of the key so that reconfiguring a machine's BMC to
// a different address invalidates the entry, and a stale result is not reused
// against the new endpoint.
func (factory *ClientFactory) redfishAvailable(ctx context.Context, machineID string, ipmiInfo *specs.BMCConfigurationSpec_IPMI, logger *zap.Logger) bool {
	factory.redfishAvailabilityMu.Lock()
	defer factory.redfishAvailabilityMu.Unlock()

	address := ipmiInfo.Address
	key := machineID + "@" + address

	available, ok := factory.redfishAvailabilityCache[key]
	if ok {
		return available
	}

	logger.Debug("probe redfish availability", zap.String("machine", machineID), zap.String("address", address))

	redfishClient := redfish.NewClient(factory.options.RedfishOptions, address, ipmiInfo.Username, ipmiInfo.Password, logger)

	if _, err := redfishClient.IsPoweredOn(ctx); err != nil {
		logger.Debug("redfish is not available", zap.String("machine", machineID), zap.String("address", address), zap.Error(err))

		factory.redfishAvailabilityCache[key] = false

		return false
	}

	logger.Debug("redfish is available", zap.String("machine", machineID), zap.String("address", address))

	factory.redfishAvailabilityCache[key] = true

	return true
}

type loggingClient struct {
	client       backend
	logger       *zap.Logger
	capabilities Capabilities
}

// Capabilities implements the Client interface.
func (client *loggingClient) Capabilities() Capabilities {
	return client.capabilities
}

func (client *loggingClient) Close(ctx context.Context) error {
	client.logger.Debug("close client")

	return client.client.Close(ctx)
}

func (client *loggingClient) Reboot(ctx context.Context) error {
	client.logger.Debug("reboot")

	return client.client.Reboot(ctx)
}

func (client *loggingClient) IsPoweredOn(ctx context.Context) (bool, error) {
	client.logger.Debug("is powered on")

	return client.client.IsPoweredOn(ctx)
}

func (client *loggingClient) PowerOn(ctx context.Context) error {
	client.logger.Debug("power on")

	return client.client.PowerOn(ctx)
}

func (client *loggingClient) PowerOff(ctx context.Context) error {
	client.logger.Debug("power off")

	return client.client.PowerOff(ctx)
}

func (client *loggingClient) SetPXEBootOnce(ctx context.Context, mode pxe.BootMode) error {
	client.logger.Debug("set PXE boot once", zap.String("mode", string(mode)))

	return client.client.SetPXEBootOnce(ctx, mode)
}

func (client *loggingClient) ResetBootDevice(ctx context.Context) error {
	client.logger.Debug("reset boot device")

	return client.client.ResetBootDevice(ctx)
}

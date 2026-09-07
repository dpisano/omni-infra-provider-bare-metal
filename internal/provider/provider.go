// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements the bare metal infra provider.
package provider

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/controller"
	"github.com/cosi-project/runtime/pkg/controller/runtime"
	runtimeoptions "github.com/cosi-project/runtime/pkg/controller/runtime/options"
	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/resource/protobuf"
	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/hashicorp/go-multierror"
	"github.com/siderolabs/omni/client/pkg/client"
	providercontrollers "github.com/siderolabs/omni/client/pkg/infra/controllers"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/siderolabs/omni/client/pkg/omni/resources/omni"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/constants"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/agent"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc"
	bmcapi "github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/api"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/pxe"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/config"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/controllers"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/dhcp"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/imagefactory"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/ip"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/ipxe"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/machine"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/machineconfig"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/meta"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/resources"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/rpi"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/server"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/tftp"
	tls "github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/tls"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/util"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/version"
)

//go:embed data/icon.svg
var icon []byte

// pxeBootEventChBuffer bounds how many PXE boot events can queue while the consumer is busy polling BMCs, so a burst of concurrent boots does not block the iPXE handler.
const pxeBootEventChBuffer = 1024

// Provider implements the bare metal infra provider.
type Provider struct {
	logger *zap.Logger

	options Options
}

// New creates a new Provider.
func New(options Options, logger *zap.Logger) *Provider {
	return &Provider{
		options: options,
		logger:  logger,
	}
}

// validateOptions rejects flag combinations that cannot work, before anything starts.
//
// The point of doing this here rather than where each option is used is that most of these only
// bite much later, per machine, once Omni accepts one, and read as an unrelated controller error
// at that point.
func validateOptions(options Options, pxeBootMode pxe.BootMode) error {
	if options.SecureBootEnabled {
		if pxeBootMode != pxe.BootModeUEFI {
			return errors.New("secure boot is only supported with UEFI boot mode")
		}

		if options.UseLocalBootAssets {
			return errors.New("local boot assets cannot be used with secure boot")
		}
	}

	// Agent test mode boots the agent with the test mode kernel argument, and such an agent reports
	// API-based power management instead of configuring IPMI. The provider then has to look that
	// machine's power API address up in the state directory, so without one there is nothing to
	// look it up in, and the failure surfaces only once a machine is accepted, as a per-machine
	// BMCConfigurationController error naming an empty path.
	if options.AgentTestMode && options.APIPowerMgmtStateDir == "" {
		return errors.New("--agent-test-mode requires --api-power-mgmt-state-dir, which is where each machine's power management API address is read from: " +
			"agent test mode is for the QEMU machines that qemu-up and 'talosctl cluster create' create, so drop it when running against real hardware, " +
			"where the agent configures IPMI instead")
	}

	// A path that is not there is almost always a volume that was not mounted into the container,
	// and it would otherwise go unnoticed until a machine needed it.
	if options.APIPowerMgmtStateDir != "" {
		info, err := os.Stat(options.APIPowerMgmtStateDir)

		switch {
		case err != nil:
			return fmt.Errorf("failed to read the API power management state directory given by --api-power-mgmt-state-dir: %w", err)
		case !info.IsDir():
			return fmt.Errorf("the API power management state directory given by --api-power-mgmt-state-dir is not a directory: %q", options.APIPowerMgmtStateDir)
		}
	}

	return nil
}

// Run runs the provider.
//
//nolint:gocyclo,cyclop,gocognit,maintidx
func (p *Provider) Run(ctx context.Context) error {
	pxeBootMode, err := pxe.ParseBootMode(p.options.IPMIPXEBootMode)
	if err != nil {
		return fmt.Errorf("failed to parse IPMI PXE boot mode: %w", err)
	}

	if err = validateOptions(p.options, pxeBootMode); err != nil {
		return err
	}

	if p.options.UseLocalBootAssets {
		if err = ipxe.ValidateBootAssets(p.options.BootAssetsPath, p.logger); err != nil {
			return err
		}
	}

	apiAdvertiseAddress, err := p.determineAPIAdvertiseAddress()
	if err != nil {
		return fmt.Errorf("failed to determine API advertise address: %w", err)
	}

	dhcpProxyIfaceOrIP := p.options.DHCPProxyIfaceOrIP
	if dhcpProxyIfaceOrIP == "" {
		dhcpProxyIfaceOrIP = apiAdvertiseAddress
	}

	p.logger.Info(
		"starting provider",
		zap.Any("options", p.options),
		zap.String("api_advertise_address", apiAdvertiseAddress),
		zap.String("dhcp_proxy_iface_or_ip", dhcpProxyIfaceOrIP),
	)

	omniAPIClient, err := p.buildOmniAPIClient(p.options.OmniAPIEndpoint, p.options.InsecureSkipTLSVerify)
	if err != nil {
		return fmt.Errorf("failed to build omni client: %w", err)
	}

	defer util.LogClose(omniAPIClient, p.logger)

	if err = errors.Join(
		protobuf.RegisterResource(resources.BMCConfigurationType(), &resources.BMCConfiguration{}),
		protobuf.RegisterResource(resources.MachineStatusType(), &resources.MachineStatus{}),
		protobuf.RegisterResource(resources.PowerOperationType(), &resources.PowerOperation{}),
		protobuf.RegisterResource(resources.RebootStatusType(), &resources.RebootStatus{}),
		protobuf.RegisterResource(resources.TLSConfigType(), &resources.TLSConfig{}),
		protobuf.RegisterResource(resources.WipeStatusType(), &resources.WipeStatus{}),
	); err != nil {
		return fmt.Errorf("failed to register resources: %w", err)
	}

	cosiRuntime, err := BuildCOSIRuntime(omniAPIClient.Omni().State(), p.options.EnableResourceCache, p.logger.With(zap.String("component", "cosi_runtime")))
	if err != nil {
		return fmt.Errorf("failed to build COSI runtime: %w", err)
	}

	omniState := state.WrapCore(cosiRuntime.CachedState())

	if p.options.ClearState {
		if err = p.clearState(ctx, omniState); err != nil {
			return fmt.Errorf("failed to clear state: %w", err)
		}
	}

	if err = p.ensureProviderStatus(ctx, omniState); err != nil {
		return err
	}

	var certs *tls.Certs

	if p.options.TLS.Enabled {
		if certs, err = tls.Initialize(ctx, omniState, apiAdvertiseAddress, p.options.TLS, p.logger); err != nil {
			return fmt.Errorf("failed to initialize TLS: %w", err)
		}
	}

	agentConnectionEventCh := make(chan controllers.AgentConnectionEvent)

	imageFactoryClient, err := imagefactory.NewClient(p.options.ImageFactoryBaseURL, p.options.ImageFactoryPXEBaseURL,
		p.options.AgentModeTalosVersion, p.options.SecureBootEnabled, p.logger.With(zap.String("component", "image_factory_client")))
	if err != nil {
		return fmt.Errorf("failed to create image factory client: %w", err)
	}

	machineConfig, err := machineconfig.Build(ctx, omniState, certs, p.options.ExtraMachineConfigPath)
	if err != nil {
		return fmt.Errorf("failed to build machine config: %w", err)
	}

	pxeBootEventCh := make(chan controllers.PXEBootEvent, pxeBootEventChBuffer)

	// shared by the iPXE handler and the controllers, so they all agree on what a machine should boot
	bootOptions := machine.BootOptions{
		AlwaysNetboot: p.options.AlwaysNetboot,
	}

	ipxeHandler, err := ipxe.NewHandler(
		imageFactoryClient, machineConfig, omniState, pxeBootEventCh,
		ipxe.HandlerOptions{
			APIAdvertiseAddress: apiAdvertiseAddress,
			APIPort:             p.options.APIPort,
			TLS:                 p.options.TLS,
			UseLocalBootAssets:  p.options.UseLocalBootAssets,
			BootAssetsPath:      p.options.BootAssetsPath,
			AgentTestMode:       p.options.AgentTestMode,
			BootFromDiskMethod:  p.options.BootFromDiskMethod,
			Boot:                bootOptions,
		},
		p.logger.With(zap.String("component", "ipxe_handler")),
	)
	if err != nil {
		return fmt.Errorf("failed to create iPXE handler: %w", err)
	}

	configHandler, err := config.NewHandler(machineConfig, p.logger.With(zap.String("component", "config_handler")))
	if err != nil {
		return fmt.Errorf("failed to create config handler: %w", err)
	}

	parsedMachineLabels, err := p.parseLabels()
	if err != nil {
		return fmt.Errorf("failed to parse machine labels: %w", err)
	}

	bootFiles, err := p.bootFiles(ipxeHandler.PatchedFiles())
	if err != nil {
		return err
	}

	bmcClientFactory := bmc.NewClientFactory(bmc.ClientFactoryOptions{
		RedfishOptions: p.options.Redfish,
	})
	tftpServer := tftp.NewServer(p.options.APIListenAddress, bootFiles, p.logger.With(zap.String("component", "tftp_server")))
	bmcAPIAddressReader := bmcapi.NewAddressReader(p.options.APIPowerMgmtStateDir)
	agentClient := agent.NewClient(agentConnectionEventCh, p.options.AgentClient, p.logger.With(zap.String("component", "agent_client"))) //nolint:contextcheck // false positive

	assetsDir := ""
	if p.options.UseLocalBootAssets {
		assetsDir = p.options.BootAssetsPath
	}

	srvr := server.New(ctx, p.options.APIListenAddress, p.options.APIPort, p.options.TLS.APIPort, assetsDir, certs, configHandler, ipxeHandler, bootFiles,
		agentClient.TunnelServiceServer(), p.logger.With(zap.String("component", "server")))

	healthCheckController, err := providercontrollers.NewProviderHealthStatusController(meta.ProviderID.String(), providercontrollers.ProviderHealthStatusOptions{})
	if err != nil {
		return fmt.Errorf("failed to create health check controller: %w", err)
	}

	if err = cosiRuntime.RegisterController(healthCheckController); err != nil {
		return fmt.Errorf("failed to register health check controller: %w", err)
	}

	for _, qController := range []controller.QController{
		controllers.NewMachineStatusController(bmcClientFactory, agentClient, agentConnectionEventCh, pxeBootEventCh, 30*time.Second),
		controllers.NewInfraMachineStatusController(parsedMachineLabels),
		controllers.NewBMCConfigurationController(agentClient, bmcAPIAddressReader, controllers.BMCConfigurationControllerOptions{
			AllowMachinesWithoutBMC: p.options.AllowMachinesWithoutBMC,
		}),
		controllers.NewPowerOperationController(time.Now, bmcClientFactory, p.options.MinRebootInterval, pxeBootMode, bootOptions),
		controllers.NewRebootStatusController(bmcClientFactory, agentClient, p.options.MinRebootInterval, pxeBootMode, controllers.RebootStatusControllerOptions{
			BootOptions: bootOptions,
		}),
		controllers.NewWipeStatusController(agentClient),
	} {
		if err = cosiRuntime.RegisterQController(qController); err != nil {
			return fmt.Errorf("failed to register QController: %w", err)
		}
	}

	components := []component{
		{cosiRuntime.Run, "COSI runtime"},
		{srvr.Run, "server"},
		{tftpServer.Run, "TFTP server"},
	}

	if !p.options.DisableDHCPProxy {
		dhcpProxy := dhcp.NewProxy(apiAdvertiseAddress, p.options.APIPort, dhcpProxyIfaceOrIP, p.options.DisableDHCPProxyBroadcast, p.logger.With(zap.String("component", "dhcp_proxy")))

		components = append(components, component{dhcpProxy.Run, "DHCP proxy"})
	}

	return p.runComponents(ctx, components)
}

// BuildCOSIRuntime creates a new COSI runtime wrapping the given state, registering the provider-specific resources.
func BuildCOSIRuntime(state state.State, enableResourceCache bool, logger *zap.Logger) (*runtime.Runtime, error) {
	var options []runtimeoptions.Option

	if enableResourceCache {
		options = append(
			options,
			safe.WithResourceCache[*resources.BMCConfiguration](),
			safe.WithResourceCache[*resources.MachineStatus](),
			safe.WithResourceCache[*resources.PowerOperation](),
			safe.WithResourceCache[*resources.RebootStatus](),
			safe.WithResourceCache[*resources.TLSConfig](),
			safe.WithResourceCache[*resources.WipeStatus](),
		)
	}

	cosiRuntime, err := runtime.NewRuntime(state, logger, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create runtime: %w", err)
	}

	return cosiRuntime, nil
}

// RegisterResources registers the provider-specific resources' protobuf marshaler/unmarshalers.
func RegisterResources() error {
	return errors.Join(
		protobuf.RegisterResource(resources.BMCConfigurationType(), &resources.BMCConfiguration{}),
		protobuf.RegisterResource(resources.MachineStatusType(), &resources.MachineStatus{}),
		protobuf.RegisterResource(resources.PowerOperationType(), &resources.PowerOperation{}),
		protobuf.RegisterResource(resources.RebootStatusType(), &resources.RebootStatus{}),
		protobuf.RegisterResource(resources.TLSConfigType(), &resources.TLSConfig{}),
		protobuf.RegisterResource(resources.WipeStatusType(), &resources.WipeStatus{}),
	)
}

func (p *Provider) ensureProviderStatus(ctx context.Context, st state.State) error {
	populate := func(res *infra.ProviderStatus) {
		res.Metadata().Labels().Set(omni.LabelIsStaticInfraProvider, "")

		res.TypedSpec().Value.Name = p.options.Name
		res.TypedSpec().Value.Description = p.options.Description
		res.TypedSpec().Value.Icon = base64.RawStdEncoding.EncodeToString(icon)
		res.TypedSpec().Value.Version = version.Tag
	}

	providerStatus := infra.NewProviderStatus(meta.ProviderID.String())

	populate(providerStatus)

	if err := st.Create(ctx, providerStatus); err != nil {
		if !state.IsConflictError(err) {
			return err
		}

		if _, err = safe.StateUpdateWithConflicts(ctx, st, providerStatus.Metadata(), func(res *infra.ProviderStatus) error {
			populate(res)

			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

type component struct {
	run  func(context.Context) error
	name string
}

// runComponents runs the long-running components in their own goroutines.
//
// It will terminate all components when one of them terminates, irrespective of whether it terminates with an error.
func (p *Provider) runComponents(ctx context.Context, components []component) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)

	for _, comp := range components {
		logger := p.logger.With(zap.String("component", comp.name))

		eg.Go(func() error {
			defer cancel() // cancel the parent context, so all other components are also stopped even if this one does not return an error

			logger.Info("start component")

			if err := comp.run(ctx); err != nil {
				logger.Error("failed to run component", zap.Error(err))

				return err
			}

			logger.Info("component stopped")

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return fmt.Errorf("failed to run components: %w", err)
	}

	return nil
}

func (p *Provider) determineAPIAdvertiseAddress() (string, error) {
	if p.options.APIAdvertiseAddress != "" {
		return p.options.APIAdvertiseAddress, nil
	}

	routableIPs, err := ip.RoutableIPs()
	if err != nil {
		return "", fmt.Errorf("failed to get routable IPs: %w", err)
	}

	if len(routableIPs) != 1 {
		return "", fmt.Errorf(`expected exactly one routable IP, got %d: %v. specify API advertise address explicitly`, len(routableIPs), routableIPs)
	}

	return routableIPs[0], nil
}

// buildOmniAPIClient creates a new Omni API client.
func (p *Provider) buildOmniAPIClient(endpoint string, insecureSkipTLSVerify bool) (*client.Client, error) {
	if endpoint == "" {
		return nil, errors.New("missing Omni API endpoint")
	}

	serviceAccountKey := os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")

	cliOpts := []client.Option{
		client.WithInsecureSkipTLSVerify(insecureSkipTLSVerify),
	}

	if serviceAccountKey != "" {
		cliOpts = append(cliOpts, client.WithServiceAccount(serviceAccountKey))
	}

	return client.New(endpoint, cliOpts...)
}

// clearState clears the persistent state of this provider. Useful for debugging purposes.
func (p *Provider) clearState(ctx context.Context, st state.State) error {
	if !constants.IsDebugBuild {
		p.logger.Warn("clear state is requested, but this is not a debug build, skipping")

		return nil
	}

	var resourcesToDestroy []resource.Resource

	for _, md := range []*resource.Metadata{
		resources.NewBMCConfiguration("").Metadata(),
		resources.NewMachineStatus("").Metadata(),
		resources.NewPowerOperation("").Metadata(),
		resources.NewRebootStatus("").Metadata(),
		resources.NewTLSConfig().Metadata(),
		resources.NewWipeStatus("").Metadata(),
	} {
		list, err := st.List(ctx, md)
		if err != nil {
			return fmt.Errorf("failed to list bare metal machinees: %w", err)
		}

		resourcesToDestroy = append(resourcesToDestroy, list.Items...)
	}

	var errs error

	for _, item := range resourcesToDestroy {
		res, getErr := st.Get(ctx, item.Metadata())
		if getErr != nil {
			errs = multierror.Append(errs, getErr)

			continue
		}

		if destroyErr := st.Destroy(ctx, item.Metadata(), state.WithDestroyOwner(res.Metadata().Owner())); destroyErr != nil {
			errs = multierror.Append(errs, destroyErr)

			continue
		}
	}

	if errs != nil {
		return fmt.Errorf("failed to clear state: %w", errs)
	}

	p.logger.Info("state cleared")

	return nil
}

// bootFiles returns everything served over TFTP and under the HTTP /tftp/ path: the patched iPXE
// binaries, plus the Raspberry Pi firmware when the operator supplied it.
//
// The two sets are merged rather than served separately because a Raspberry Pi fetches its firmware
// from the same TFTP server that every other machine chainloads iPXE from.
func (p *Provider) bootFiles(ipxeFiles map[string][]byte) (map[string][]byte, error) {
	if p.options.RPiFirmwarePath == "" {
		return ipxeFiles, nil
	}

	firmware, err := rpi.Load(p.options.RPiFirmwarePath, p.logger.With(zap.String("component", "rpi_firmware")))
	if err != nil {
		return nil, err
	}

	files := maps.Clone(ipxeFiles)

	for name, contents := range firmware {
		if _, conflict := files[name]; conflict {
			return nil, fmt.Errorf("firmware file %q for Raspberry Pi collides with an iPXE boot file of the same name", name)
		}

		files[name] = contents
	}

	return files, nil
}

func (p *Provider) parseLabels() (map[string]string, error) {
	labels := make(map[string]string, len(p.options.MachineLabels))

	for _, l := range p.options.MachineLabels {
		parts := strings.Split(l, "=")
		if len(parts) > 2 {
			return nil, fmt.Errorf("malformed label %s", l)
		}

		value := ""

		if len(parts) > 1 {
			value = parts[1]
		}

		labels[parts[0]] = value
	}

	return labels, nil
}

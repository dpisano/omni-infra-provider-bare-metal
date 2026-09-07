# omni-infra-provider-bare-metal

This repo contains the code for the Omni Bare Metal Infra Provider.
If you would like to deploy the provider in your environment please [see the official documentation](https://omni.siderolabs.com/tutorials/setting-up-the-bare-metal-infrastructure-provider).

This is a fork of [siderolabs/omni-infra-provider-bare-metal](https://github.com/siderolabs/omni-infra-provider-bare-metal).
It adds support for [machines with no BMC](#machines-without-a-bmc), whose power a human controls, and for [Raspberry Pi network boot](#raspberry-pi-network-boot).
Everything else works as upstream documents it.

## Requirements

To run the provider, you need:

- A running Omni instance
- An infra provider created in Omni, matching the ID you'll use with this provider (`bare-metal` by default).
  To create it, run:

  ```bash
  omnictl infraprovider create bare-metal
  ```

  Replace `bare-metal` with your desired provider ID.
- A DHCP server: This provider runs a DHCP proxy to provide DHCP responses for iPXE boot, so a DHCP server must be running in the same network as the provider.
- Access to an [Image Factory](https://www.talos.dev/v1.8/learn-more/image-factory/).

## Machines without a BMC

Some machines have no IPMI and no Redfish at all, so there is nothing for the provider to talk to out of band.
By default the provider fails to configure one, and it never becomes ready to use, sitting in agent mode instead.
Pass `--allow-machines-without-bmc` to record it as manually powered, which lets it join the pool like any other machine.

What changes for such a machine:

- **You power it on and off.**
  The provider never does, so when Omni allocates the machine to a cluster it waits for you to turn it on.
- **Its power state is inferred from whether its agent answers**, rather than read from a BMC.
  An unreachable agent is not taken to mean the machine is off, since it may be running Talos without the agent.
- **Reboots go over the agent**, which only works while the machine runs in agent mode.
  A machine that needs rebooting outside agent mode is left for you to power-cycle by hand.
- **It must be configured to network boot first** in its own firmware.
  The provider cannot set a one-time boot device for it, so the boot order is the only thing keeping the provider in control of what it boots.

Machines that do have a BMC keep using it, and the two kinds can be mixed in one deployment: the flag only changes what happens to a machine whose agent reports no power management.

The natural companion is `--always-netboot`, which stops the provider from ever handing an installed machine off to its disk, and serves it the cluster's Talos version over the network on every boot instead.
It is for machines whose firmware cannot boot the installed system, a Raspberry Pi being the case that needs it.
The disk still holds the machine's state, and only the kernel and initramfs come from the provider.
The cost is that the provider becomes a hard dependency of every boot: while it is down, a machine that reboots does not come back up.

## Raspberry Pi network boot

A Raspberry Pi does not network boot the way a PC does, so it takes an extra stage before the normal flow applies.
Its on-board bootloader fetches a fixed set of files over TFTP by name and runs the kernel `config.txt` names, which here is U-Boot.
Only then does the board make an ordinary PXE request, which the provider answers like any other arm64 machine.

To boot one:

1. Enable network boot in the board's EEPROM, ordered before the SD card.
   This is a one-time setup done with `raspi-config` or `rpi-eeprom-config`.
2. Populate a directory with the boot files and point `--rpi-firmware-path` at it.
   See [`hack/rpi`](hack/rpi) for a `config.txt` to start from and where each file comes from.
   These files are not shipped with the provider, as the GPU firmware is proprietary Broadcom code rather than MPL-2.0.
   The provider refuses to start when a required file is missing, so a directory that would leave a board hanging is caught up front.
3. Run the provider with `--always-netboot`, and with `--allow-machines-without-bmc` unless the board has some form of external power control.
   `--always-netboot` is not optional here: Omni installs Talos without a board overlay, so the installed disk has no bootloader the Pi firmware can start.

Without `--rpi-firmware-path`, the provider recognises a board but leaves it alone rather than offering it a boot it cannot serve, and logs a warning naming the flag the first time it sees each board.
A board that was offered a boot with no files behind it just loops fetching `start4.elf` and its siblings, which shows up only as a run of `file not found` in the log.

Only the Raspberry Pi 4 and CM4 are supported.
A Pi 3 additionally needs `bootcode.bin`, which on a Pi 4 lives in the on-board EEPROM.

A board is recognised by the OUI of its MAC address, because its bootloader is otherwise indistinguishable from a legacy x86 BIOS PXE client.
A Pi booting over a USB network adapter is therefore not detected as one.

Agent mode images for arm64 are built without the x86 hardware firmware extensions the amd64 images carry, none of which a Pi can use.
This makes the arm64 agent mode initramfs roughly a quarter smaller.

## Container images

Images are published to `ghcr.io/<repository owner>/omni-infra-provider-bare-metal`, built by the same `make image-provider` target used locally, for `linux/amd64` and `linux/arm64`.
Every commit to `main` publishes one, tagged with `git describe` output such as `v0.12.0-7-g63cc659`.
A pull request builds the image without pushing it, only as a check that it still builds, after its unit tests pass.

Old images are pruned weekly, keeping every release and the ten most recent builds.
To see what would go without deleting anything, run `hack/prune-images.sh --owner <owner>`, which is a dry run unless given `--delete`.

## Releases

This fork does not version independently.
It cuts a release whenever upstream cuts one, named after the upstream release it corresponds to, so upstream `v0.13.0` becomes `v0.13.0` here and the image is tagged `v0.13.0`.
Because the tag points at this fork's `main`, the release carries upstream's release plus what this fork adds, and it is a different commit than upstream's tag of the same name.

A release is only cut once `main` actually contains the upstream release commit, so it waits for the weekly upstream sync pull request to be reviewed and merged.
Nothing merges automatically.

A release tracks `main` rather than a fixed commit, so it is redone whenever a commit lands on `main` afterwards: the tag moves onto the newer `main` and the image under it is replaced.
A version here therefore names the newest `main` carrying that upstream release, not the state of `main` on the day upstream cut it, and the same version tag can give you different bits over time.
The commit a release currently names is in its notes, so pin to that commit's own image tag if you need bits that never change under you.

Upstream's own CI is not used here, as every job in it needs Sidero Labs infrastructure to run.
See the GitHub workflows section of [AGENTS.md](AGENTS.md) for what replaces it.

## Development

For local development using Talos running on QEMU, follow these steps:

1. Set up a `buildx` builder instance with host network access, if you don't have one already:

   ```bash
   docker buildx create --driver docker-container --driver-opt network=host --name local1 --buildkitd-flags '--allow-insecure-entitlement security.insecure' --use
   ```

2. Start a local image registry if you don't have one running:

   ```bash
   docker run -d -p 5005:5000 --restart always --name local registry:2
   ```

3. Build `qemu-up` command line tool, and use it to start some QEMU machines:

   ```bash
   make qemu-up
   sudo -E _out/qemu-up-linux-amd64
   ```

   If the machine firmware does not accept ProxyDHCP offers (notably EDK2, the UEFI firmware QEMU uses),
   pass `--pxe-boot-via-dhcpd` to hand out the PXE boot info from the provisioner's own DHCP server instead.
   The option only applies when the machines are created.

   Note: everything in this repo also builds and runs natively without docker.
   On a fresh clone, run `make fetch-source-assets` once to fetch the iPXE binaries which get embedded into the provider binary, then regular `go build` / `go run` work.

4. (Optional) If you have made local changes to the [Talos Metal agent](https://github.com/siderolabs/talos-metal-agent), follow these steps to use your local version:
    1. Build and push Talos Metal Agent boot assets image following [these instructions](https://github.com/siderolabs/talos-metal-agent/blob/main/README.md).
    2. Replace the `ghcr.io/siderolabs/talos-metal-agent-boot-assets` image reference in [.kres.yaml](.kres.yaml) with your built image,
       e.g., `127.0.0.1:5005/siderolabs/talos-metal-agent-boot-assets:v1.9.0-agent-v0.1.0-beta.1-1-gbf1282b-dirty`.
    3. Re-kres the project to propagate this change into `Dockerfile`:

       ```bash
       make rekres
       ```

5. Build a local provider image:

   ```bash
   make image-provider PLATFORM=linux/amd64 REGISTRY=127.0.0.1:5005 PUSH=true TAG=local-dev
   docker pull 127.0.0.1:5005/siderolabs/omni-infra-provider-bare-metal:local-dev
   ```

6. Start the provider with your Omni API address and the infra provider service account credentials:

   ```bash
   export OMNI_ENDPOINT=<your-omni-api-address>
   export OMNI_SERVICE_ACCOUNT_KEY=<your-omni-service-account-key>

   docker run --name=omni-bare-metal-provider --network host --rm -it \
     -v "$HOME/.talos/clusters/bare-metal:/api-power-mgmt-state:ro" \
     -e OMNI_ENDPOINT -e OMNI_SERVICE_ACCOUNT_KEY \
     127.0.0.1:5005/siderolabs/omni-infra-provider-bare-metal:local-dev \
     --insecure-skip-tls-verify \
     --api-advertise-address=<provider-ip-to-advertise> \
     --use-local-boot-assets \
     --agent-test-mode \
     --api-power-mgmt-state-dir=/api-power-mgmt-state \
     --dhcp-proxy-iface-or-ip=172.29.0.1 \
     --debug
   ```

   Important flags:
    - `--use-local-boot-assets`: Makes the provider serve the boot assets from a local directory, given by `--boot-assets-path` and defaulting to the `/assets` baked into the provider image.
      This is useful for testing local Talos Metal Agent boot assets.
      The directory is validated at startup, and the provider refuses to start when no architecture has a complete set of assets.
      Omit this flag to use the upstream agent version, which will forward agent mode PXE boot requests to the image factory.
    - `--agent-test-mode`: Boots the agent in test mode when booting a Talos node in agent mode, enabling API-based power management instead of IPMI/RedFish.
      This is necessary for QEMU development,
      as it uses the power management API run by the `talosctl cluster create` command.
    - The volume mount `-v "$HOME/.talos/clusters/bare-metal:/api-power-mgmt-state:ro"`
      mounts the directory containing API-based power management state information generated by `talosctl cluster create`.
    - `--api-power-mgmt-state-dir`: Specifies where to read the API power management address of the nodes.
    - `--dhcp-proxy-iface-or-ip`: Specifies the IP address or interface name for running the DHCP proxy
      (e.g., the IP address of the QEMU bridge interface).
      The tool `qemu-up` uses the subnet `172.29.0.0/24` by default, and the bridge IP address on the host is `172.29.0.1`.

7. When you are done with the development/testing, destroy all QEMU machines and their network bridge:

   ```bash
   sudo -E _out/qemu-up-linux-amd64 --destroy
   ```

## Integration Tests

**These are manual integration tests meant to be run against a real BMC endpoint.**
**They do NOT run in CI.**

They exercise BMC operations (power on/off, reboot, PXE boot, power state queries) and are behind separate build tags, excluded from regular test runs.

The tests will power cycle the target machine, so make sure it is safe to do so.

### IPMI

```bash
go test -tags integration_ipmi -v -timeout 30m ./internal/integration/... \
  -bmc-address <bmc-ip> -bmc-username <user> -bmc-password <pass>
```

Additional flag: `-ipmi-port` (default 623).

### Redfish

```bash
go test -tags integration_redfish -v -timeout 30m ./internal/integration/... \
  -bmc-address <bmc-ip> -bmc-username <user> -bmc-password <pass>
```

Additional flags: `-redfish-port` (default 443), `-redfish-use-https` (default true), `-redfish-insecure-skip-tls` (default true), `-redfish-set-boot-source-override-mode` (default true).

### Both

```bash
go test -tags "integration_ipmi,integration_redfish" -v -timeout 60m ./internal/integration/... \
  -bmc-address <bmc-ip> -bmc-username <user> -bmc-password <pass>
```

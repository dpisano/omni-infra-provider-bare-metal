# Raspberry Pi network boot files

A Raspberry Pi does not network boot the way a PC does.
Its on-board bootloader speaks just enough ProxyDHCP to find a TFTP server, then fetches a fixed set of files by name before it can run anything.
Only once U-Boot is running does the board make an ordinary PXE request that the provider answers like any other machine.

Point `--rpi-firmware-path` at a directory holding these files.
They are not shipped with the provider: the GPU firmware is proprietary Broadcom code under the Raspberry Pi license, not this project's MPL-2.0.

## What the directory needs

| File | Where it comes from |
| --- | --- |
| `config.txt` | `config.txt` in this directory, as a starting point |
| `start4.elf` | the `boot/` directory of <https://github.com/raspberrypi/firmware> |
| `fixup4.dat` | the same place |
| `u-boot.bin` | a U-Boot build for `rpi_arm64_defconfig`, named to match `kernel=` in `config.txt` |
| `bcm2711-rpi-4-b.dtb` | the same firmware repository, matching the board |

The provider refuses to start when `config.txt`, `start4.elf` or `fixup4.dat` are missing, so a directory that would leave a board hanging is caught up front.

Only the Raspberry Pi 4 and CM4 are supported.
A Pi 3 additionally needs `bootcode.bin`, which on a Pi 4 lives in the on-board EEPROM instead.

## On the board itself

The board's EEPROM has to have network boot enabled and ordered before the SD card, which is a one-time setup done with `raspi-config` or `rpi-eeprom-config`.

A Pi is recognised by the OUI of its MAC address, so a Pi booting over a USB network adapter is not detected as one.

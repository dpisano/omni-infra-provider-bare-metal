// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package rpi serves the boot files the Raspberry Pi on-board bootloader fetches for itself.
//
// A Raspberry Pi does not network boot the way a PC does. Its bootloader speaks just enough
// ProxyDHCP to find a TFTP server, and then fetches a fixed set of files by name: the GPU firmware,
// a config, and a kernel, which here is U-Boot. Only once U-Boot is running does the machine make a
// second, ordinary PXE request that the rest of this provider already understands.
//
// The files are not redistributable as part of this provider, since the GPU firmware is proprietary
// Broadcom code under the Raspberry Pi license rather than MPL-2.0. The operator populates a
// directory with them and points --rpi-firmware-path at it.
package rpi

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"go.uber.org/zap"
)

// maxFileSize bounds a single firmware file, as they are all held in memory to be served over TFTP.
// The real files are a few megabytes at most, so anything larger means the directory is not what
// the operator thinks it is.
const maxFileSize = 64 << 20

// requiredFiles are the files a Raspberry Pi 4 or CM4 must find over TFTP to get as far as running
// the kernel named by config.txt.
//
// Note that bootcode.bin is deliberately not required: on a Pi 4 that stage lives in the on-board
// EEPROM, unlike on a Pi 3. Older boards are not supported.
var requiredFiles = []string{
	"config.txt",
	"start4.elf",
	"fixup4.dat",
}

// Load reads the Raspberry Pi firmware directory into memory, keyed by the name the bootloader
// requests each file under.
//
// It fails when a required file is missing, so a misconfigured directory is caught at startup
// rather than when a board first tries to boot and silently hangs.
func Load(dir string, logger *zap.Logger) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read the Raspberry Pi firmware directory: %w", err)
	}

	files := make(map[string][]byte, len(entries))

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() {
			logger.Debug("skip directory in the Raspberry Pi firmware directory", zap.String("name", name))

			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil, fmt.Errorf("failed to stat %q: %w", name, infoErr)
		}

		if !info.Mode().IsRegular() {
			logger.Debug("skip non-regular file in the Raspberry Pi firmware directory", zap.String("name", name))

			continue
		}

		if info.Size() > maxFileSize {
			return nil, fmt.Errorf("firmware file %q is %d bytes, which exceeds the %d byte limit", name, info.Size(), maxFileSize)
		}

		contents, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			return nil, fmt.Errorf("failed to read %q: %w", name, readErr)
		}

		files[name] = contents
	}

	var missing []string

	for _, required := range requiredFiles {
		if _, ok := files[required]; !ok {
			missing = append(missing, required)
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("firmware directory %q is missing %v; a Raspberry Pi 4 or CM4 needs at least %v", dir, missing, requiredFiles)
	}

	logger.Info("loaded Raspberry Pi firmware", zap.String("path", dir), zap.Strings("files", slices.Sorted(maps.Keys(files))))

	return files, nil
}

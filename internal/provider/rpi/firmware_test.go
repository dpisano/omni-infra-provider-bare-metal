// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package rpi_test

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/rpi"
)

// writeFirmwareDir builds a directory holding the given files, on top of the required set unless
// skipRequired is set.
func writeFirmwareDir(t *testing.T, extra map[string]string, skipRequired bool) string {
	t.Helper()

	dir := t.TempDir()

	files := map[string]string{}

	if !skipRequired {
		files["config.txt"] = "kernel=u-boot.bin\narm_64bit=1\n"
		files["start4.elf"] = "gpu firmware"
		files["fixup4.dat"] = "gpu fixups"
	}

	maps.Copy(files, extra)

	for name, contents := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600))
	}

	return dir
}

func TestLoad(t *testing.T) {
	t.Parallel()

	t.Run("a complete directory is loaded and keyed by file name", func(t *testing.T) {
		t.Parallel()

		dir := writeFirmwareDir(t, map[string]string{
			"u-boot.bin":          "u-boot",
			"bcm2711-rpi-4-b.dtb": "device tree",
		}, false)

		files, err := rpi.Load(dir, zaptest.NewLogger(t))
		require.NoError(t, err)

		assert.Len(t, files, 5)
		assert.Equal(t, []byte("u-boot"), files["u-boot.bin"])
		assert.Equal(t, []byte("gpu firmware"), files["start4.elf"])
		assert.Contains(t, string(files["config.txt"]), "kernel=u-boot.bin")
	})

	t.Run("a missing required file fails at load rather than at boot", func(t *testing.T) {
		t.Parallel()

		dir := writeFirmwareDir(t, map[string]string{"config.txt": "kernel=u-boot.bin\n"}, true)

		_, err := rpi.Load(dir, zaptest.NewLogger(t))
		require.Error(t, err)

		assert.Contains(t, err.Error(), "start4.elf")
		assert.Contains(t, err.Error(), "fixup4.dat")
	})

	t.Run("a missing directory is reported", func(t *testing.T) {
		t.Parallel()

		_, err := rpi.Load(filepath.Join(t.TempDir(), "does-not-exist"), zaptest.NewLogger(t))
		require.Error(t, err)

		assert.Contains(t, err.Error(), "failed to read the Raspberry Pi firmware directory")
	})

	t.Run("subdirectories are skipped rather than failing the load", func(t *testing.T) {
		t.Parallel()

		dir := writeFirmwareDir(t, nil, false)
		require.NoError(t, os.Mkdir(filepath.Join(dir, "overlays"), 0o700))

		files, err := rpi.Load(dir, zaptest.NewLogger(t))
		require.NoError(t, err)

		assert.Len(t, files, 3)
		assert.NotContains(t, files, "overlays")
	})
}

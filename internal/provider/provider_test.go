// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider"
	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/pxe"
)

func TestValidateOptions(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	notADir := filepath.Join(stateDir, "file")
	require.NoError(t, os.WriteFile(notADir, nil, 0o600))

	tests := []struct {
		name        string
		wantErr     string
		pxeBootMode pxe.BootMode
		options     provider.Options
	}{
		{
			name:        "the defaults are fine",
			pxeBootMode: pxe.BootModeUEFI,
		},
		{
			name:        "secure boot needs UEFI",
			options:     provider.Options{SecureBootEnabled: true},
			pxeBootMode: pxe.BootModeBIOS,
			wantErr:     "secure boot is only supported with UEFI boot mode",
		},
		{
			name:        "secure boot rules out local boot assets",
			options:     provider.Options{SecureBootEnabled: true, UseLocalBootAssets: true},
			pxeBootMode: pxe.BootModeUEFI,
			wantErr:     "local boot assets cannot be used with secure boot",
		},
		{
			// this is the combination that otherwise fails per machine, long after startup, as
			// "failed to read directory : open : no such file or directory"
			name:        "agent test mode without a state directory",
			options:     provider.Options{AgentTestMode: true},
			pxeBootMode: pxe.BootModeUEFI,
			wantErr:     "--agent-test-mode requires --api-power-mgmt-state-dir",
		},
		{
			name:        "agent test mode with a state directory",
			options:     provider.Options{AgentTestMode: true, APIPowerMgmtStateDir: stateDir},
			pxeBootMode: pxe.BootModeUEFI,
		},
		{
			name:        "a state directory that is not there",
			options:     provider.Options{APIPowerMgmtStateDir: filepath.Join(stateDir, "missing")},
			pxeBootMode: pxe.BootModeUEFI,
			wantErr:     "failed to read the API power management state directory",
		},
		{
			name:        "a state directory that is a file",
			options:     provider.Options{APIPowerMgmtStateDir: notADir},
			pxeBootMode: pxe.BootModeUEFI,
			wantErr:     "is not a directory",
		},
		{
			// real hardware without the test mode flag is untouched, whatever the state dir is
			name:        "no agent test mode needs no state directory",
			options:     provider.Options{},
			pxeBootMode: pxe.BootModeUEFI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := provider.ValidateOptions(tt.options, tt.pxeBootMode)

			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

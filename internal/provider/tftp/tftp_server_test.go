// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package tftp_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/tftp"
)

func TestStripBoardDirectory(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		request    string
		want       string
		isBoardDir bool
	}{
		{
			name:       "serial number directory",
			request:    "1a2b3c4d/start4.elf",
			want:       "start4.elf",
			isBoardDir: true,
		},
		{
			name:       "uppercase serial number directory",
			request:    "1A2B3C4D/config.txt",
			want:       "config.txt",
			isBoardDir: true,
		},
		{
			name:       "MAC address directory",
			request:    "dc-a6-32-11-22-33/start4.elf",
			want:       "start4.elf",
			isBoardDir: true,
		},
		{
			name:    "a plain file name is left alone",
			request: "snp-arm64.efi",
			want:    "snp-arm64.efi",
		},
		{
			// this is a real key in the served file map, and must never be mistaken for a board directory
			name:    "an architecture directory is left alone",
			request: "arm64/snp.efi",
			want:    "arm64/snp.efi",
		},
		{
			name:    "a too-short hex directory is left alone",
			request: "1a2b3c/start4.elf",
			want:    "1a2b3c/start4.elf",
		},
		{
			name:    "a too-long hex directory is left alone",
			request: "1a2b3c4d5e/start4.elf",
			want:    "1a2b3c4d5e/start4.elf",
		},
		{
			name:    "a non-hex directory of the right length is left alone",
			request: "someword/start4.elf",
			want:    "someword/start4.elf",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, isBoardDir := tftp.StripBoardDirectory(test.request)

			assert.Equal(t, test.want, got)
			assert.Equal(t, test.isBoardDir, isBoardDir)
		})
	}
}

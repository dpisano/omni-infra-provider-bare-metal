// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package manual provides a BMC backend for machines that have no BMC at all.
//
// Such a machine is powered by a human, so there is nothing to talk to out of band: the provider
// cannot read its power state, power it on or off, or set a one-time boot device. Every operation
// therefore fails with ErrNotSupported, and callers are expected to consult the client capabilities
// before calling in the first place.
package manual

import (
	"context"
	"errors"

	"github.com/siderolabs/omni-infra-provider-bare-metal/internal/provider/bmc/pxe"
)

// ErrNotSupported is returned by every operation, as a machine without a BMC has no out-of-band control.
var ErrNotSupported = errors.New("machine has no BMC: power management is manual")

// Client is a BMC client for a machine without a BMC.
type Client struct{}

// NewClient creates a new manual BMC client.
func NewClient() *Client {
	return &Client{}
}

// Close implements the bmc.Client interface. There is nothing to close.
func (c *Client) Close(context.Context) error {
	return nil
}

// Reboot implements the bmc.Client interface.
func (c *Client) Reboot(context.Context) error {
	return ErrNotSupported
}

// IsPoweredOn implements the bmc.Client interface.
func (c *Client) IsPoweredOn(context.Context) (bool, error) {
	return false, ErrNotSupported
}

// PowerOn implements the bmc.Client interface.
func (c *Client) PowerOn(context.Context) error {
	return ErrNotSupported
}

// PowerOff implements the bmc.Client interface.
func (c *Client) PowerOff(context.Context) error {
	return ErrNotSupported
}

// SetPXEBootOnce implements the bmc.Client interface.
func (c *Client) SetPXEBootOnce(context.Context, pxe.BootMode) error {
	return ErrNotSupported
}

// ResetBootDevice implements the bmc.Client interface.
func (c *Client) ResetBootDevice(context.Context) error {
	return ErrNotSupported
}

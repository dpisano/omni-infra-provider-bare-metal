// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package tftp implements a TFTP server.
package tftp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/pin/tftp/v3"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Server represents the TFTP server serving iPXE binaries from memory.
type Server struct {
	logger *zap.Logger

	files map[string][]byte

	listenAddress string
}

// NewServer creates a new TFTP server serving the given files, keyed by the request path.
func NewServer(listenAddress string, files map[string][]byte, logger *zap.Logger) *Server {
	return &Server{
		listenAddress: listenAddress,
		files:         files,
		logger:        logger,
	}
}

// Run runs the TFTP server.
func (s *Server) Run(ctx context.Context) error {
	readHandler := func(filename string, rf io.ReaderFrom) error {
		return s.handleRead(filename, rf)
	}

	srv := tftp.NewServer(readHandler, nil)

	// A standard TFTP server implementation receives requests on port 69 and
	// allocates a new high port (over 1024) dedicated to that request. In single
	// port mode, the same port is used for transmit and receive. If the server
	// is started on port 69, all communication will be done on port 69.
	// This option is required since the Kubernetes service definition defines a
	// single port.
	srv.EnableSinglePort()
	srv.SetTimeout(5 * time.Second)

	eg, ctx := errgroup.WithContext(ctx)

	eg.Go(func() error {
		return srv.ListenAndServe(net.JoinHostPort(s.listenAddress, "69"))
	})

	eg.Go(func() error {
		<-ctx.Done()

		srv.Shutdown()

		return nil
	})

	return eg.Wait()
}

// boardDirectory matches the per-board directory the Raspberry Pi bootloader prefixes its requests
// with: either its serial number, which is eight hex digits, or its MAC address, depending on how
// the board's TFTP prefix is configured.
//
// It is deliberately narrow so it cannot swallow a real path such as "arm64/snp.efi".
var boardDirectory = regexp.MustCompile(`^([0-9a-fA-F]{8}|([0-9a-fA-F]{2}-){5}[0-9a-fA-F]{2})/`)

// stripBoardDirectory removes a Raspberry Pi board directory prefix from a request, reporting
// whether the name had one.
func stripBoardDirectory(name string) (string, bool) {
	prefix := boardDirectory.FindString(name)
	if prefix == "" {
		return name, false
	}

	return name[len(prefix):], true
}

// handleRead is called when a client starts file download from server.
func (s *Server) handleRead(filename string, rf io.ReaderFrom) error {
	s.logger.Info("file requested", zap.String("filename", filename))

	// normalize the request lexically, so both "snp.efi" and "/amd64/../snp.efi" resolve to the same key
	name := strings.TrimPrefix(path.Clean("/"+filename), "/")

	contents, ok := s.files[name]
	if !ok {
		// The Raspberry Pi bootloader prefixes every request with a directory naming the board,
		// e.g. "1a2b3c4d/start4.elf", so the same files are served under any such directory.
		if stripped, isBoardDir := stripBoardDirectory(name); isBoardDir {
			contents, ok = s.files[stripped]
		}
	}

	if !ok {
		s.logger.Error("file not found", zap.String("filename", filename))

		return fmt.Errorf("file %q not found", filename)
	}

	n, err := rf.ReadFrom(bytes.NewReader(contents))
	if err != nil {
		s.logger.Error("failed to send file", zap.String("filename", filename), zap.Error(err))

		return err
	}

	s.logger.Info("file sent", zap.String("filename", filename), zap.Int64("bytes", n))

	return nil
}

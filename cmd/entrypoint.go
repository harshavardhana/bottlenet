/*
 * Bottlenet (C) 2020 MinIO, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func bottlenetEntrypoint(ctx context.Context, args []string) error {
	if err := validateArgs(args); err != nil {
		return err
	}
	mainCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	//handle signals
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		<-c
		cancel()
	}()

	mesh := !serverMode && !clientMode

	if mesh {
		c.clusterType = clusterTypeMesh
		meshCtx, cancel := context.WithCancel(mainCtx)
		defer cancel()

		if len(args) > 0 {
			return peer(meshCtx, args[0])
		}
		return coordinate(meshCtx)
	}
	return nil
}

func validateArgs(args []string) error {
	err := validatePort(bottlenetPort)
	if err != nil {
		return err
	}
	if clientMode {
		if len(args) != 1 {
			return fmt.Errorf("client-network nodes must provide a coordinator address")
		}
		return validateHostPort(args[0])
	}
	if serverMode {
		if len(args) > 1 {
			return fmt.Errorf("too many arguments passed for server-network node")
		}
		if len(args) == 1 {
			return validateHostPort(args[0])
		}
		return nil
	}
	if len(args) > 1 {
		return fmt.Errorf("extra argument for mesh network. expected 1 argument only")
	}
	return nil
}

func validateHostPort(addr string) error {
	_, _, err := net.SplitHostPort(addr)
	return err
}

func validatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("port '%d' out of range (0, 65535]", port)
	}
	return nil
}

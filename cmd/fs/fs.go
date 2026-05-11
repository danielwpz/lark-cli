// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package fs

import (
	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdFS creates commands for exposing Lark resources as a local file tree.
func NewCmdFS(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fs",
		Short: "Expose Lark resources as a read-only local file tree",
	}
	cmd.AddCommand(NewCmdSync(f, nil))
	cmd.AddCommand(NewCmdMount(f, nil))
	cmd.AddCommand(NewCmdWebDAV(f, nil))
	return cmd
}

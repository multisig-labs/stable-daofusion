// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/ava-labs/blobvm/client"
	"github.com/ava-labs/blobvm/tree"
)

var resolveFileCmd = &cobra.Command{
	Use:   "resolve-file [options] <root> <output path>",
	Short: "Reads a file at a root and saves it to disk",
	RunE:  resolveFileFunc,
}

func resolveFileFunc(cmd *cobra.Command, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("expected exactly 2 argument, got %d", len(args))
	}

	filePath := args[1]
	if _, err := os.Stat(filePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("file %s already exists", filePath)
	}

	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file %s", filePath)
	}
	defer f.Close()

	root := common.HexToHash(args[0])
	cli := client.New(uri, requestTimeout)
	if err := tree.Download(context.Background(), cli, root, f); err != nil {
		return err
	}

	color.Green("resolved file %v and stored at %s", root, filePath)
	return nil
}

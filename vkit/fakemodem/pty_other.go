//go:build !linux

package main

import (
	"errors"
	"os"
)

var errLinuxOnly = errors.New("fakemodem runs in the virtual kit, on Linux")

func openPTY() (*os.File, string, error) { return nil, "", errLinuxOnly }

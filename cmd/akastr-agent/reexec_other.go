//go:build !linux

package main

import "errors"

func reexecAgent(string, string, string, int64) error {
	return errors.New("automatic process replacement is supported only on Linux")
}

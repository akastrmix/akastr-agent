//go:build !linux

package main

import "errors"

func reexecAgent(string, string, string) error {
	return errors.New("automatic process replacement is supported only on Linux")
}

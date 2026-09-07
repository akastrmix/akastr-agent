package app

import (
	"errors"
	"github.com/akastrmix/akastr-agent/internal/features/ipwatch"
	"github.com/akastrmix/akastr-agent/internal/operation"
)

func CheckIdle(stateFile, ipStateFile string, recentLimit int) error {
	if err := checkOperationIdle(stateFile, recentLimit); err != nil {
		return err
	}
	return ipwatch.CheckIdle(ipStateFile)
}

func CheckMaintenanceSafe(stateFile, ipStateFile string, recentLimit int) error {
	if err := checkOperationIdle(stateFile, recentLimit); err != nil {
		return err
	}
	return ipwatch.CheckMaintenanceSafe(ipStateFile)
}

func checkOperationIdle(stateFile string, recentLimit int) error {
	engine, err := operation.Open(operation.Options{StateFile: stateFile, RecentLimit: recentLimit})
	if err != nil {
		return err
	}
	if len(engine.Snapshot().Active) != 0 {
		return errors.New("an Agent operation is active")
	}
	return nil
}

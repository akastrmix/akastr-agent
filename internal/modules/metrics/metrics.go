// Package metrics reports what the machine is doing each minute: CPU, load,
// memory, disk, sockets, processes, the byte counters of its network
// interfaces and what kind of machine it is. It only reads /proc, /sys and
// /etc/os-release, and runs systemd-detect-virt once. Cloud turns the
// interface counters, which restart at boot, into traffic per day (Cloud
// ADR 0113), so the Agent keeps no state.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/akastrmix/akastr-agent/internal/module"
	"github.com/akastrmix/akastr-agent/internal/protocol"
)

const (
	Name       = "metrics"
	KindSample = "metrics.sample"
	interval   = time.Minute
)

// Config has no fields: the module is on or off.
type Config struct{}

func ParseConfig(raw json.RawMessage) (Config, error) {
	return protocol.DecodeStrict[Config](raw, Name)
}

// New sends metrics.sample each minute and when a session becomes ready.
func New() *module.Latest {
	s := &sampler{host: newHost()}
	return module.NewLatest(KindSample, interval, s.sample)
}

// sampler is used by one reporter goroutine alone.
type sampler struct {
	host           host
	cpu            *cpuTimes
	virtualization *string
	detected       bool
	failing        bool
}

func (s *sampler) sample(ctx context.Context) ([]byte, error) {
	data, err := s.read(ctx)
	if err != nil {
		// A file that cannot be read now usually cannot next minute either.
		if !s.failing {
			slog.Warn("metrics unreadable; samples are skipped until they read again", "error", err.Error())
		}
		s.failing = true
		return nil, err
	}
	if s.failing {
		slog.Info("metrics readable again")
		s.failing = false
	}
	return json.Marshal(data)
}

// read takes one sample. Only the fields the protocol allows to be null may
// be missing; anything else unreadable skips the sample.
func (s *sampler) read(ctx context.Context) (sample, error) {
	var data sample
	cpu, err := s.host.cpuTimes()
	if err != nil {
		return sample{}, fmt.Errorf("cpu: %w", err)
	}
	if s.cpu != nil {
		data.CPU = busy(*s.cpu, cpu)
	}
	s.cpu = &cpu
	if !s.detected {
		s.virtualization, s.detected = s.host.virtualization(ctx), true
	}
	if data.BootID, err = readLine(filepath.Join(s.host.proc, "sys", "kernel", "random", "boot_id")); err != nil {
		return sample{}, fmt.Errorf("boot id: %w", err)
	}
	if data.UptimeSeconds, err = s.host.uptime(); err != nil {
		return sample{}, fmt.Errorf("uptime: %w", err)
	}
	if data.Load, err = s.host.load(); err != nil {
		return sample{}, fmt.Errorf("load: %w", err)
	}
	if data.Memory, data.Swap, err = s.host.memory(); err != nil {
		return sample{}, fmt.Errorf("memory: %w", err)
	}
	if data.Disk, err = s.host.disk(); err != nil {
		return sample{}, fmt.Errorf("disk: %w", err)
	}
	if data.Connections, err = s.host.connections(); err != nil {
		return sample{}, fmt.Errorf("connections: %w", err)
	}
	if data.Processes, err = s.host.processes(); err != nil {
		return sample{}, fmt.Errorf("processes: %w", err)
	}
	if data.Interfaces, err = s.host.interfaces(); err != nil {
		return sample{}, fmt.Errorf("interfaces: %w", err)
	}
	if data.System, err = s.host.system(s.virtualization); err != nil {
		return sample{}, fmt.Errorf("system: %w", err)
	}
	return data, nil
}

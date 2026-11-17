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
	"sync"
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

// Sampler sends metrics.sample each minute and when a session becomes ready.
// Only the newest sample matters: one Cloud has not acknowledged yet is
// replaced by the next, never queued.
type Sampler struct {
	host host
	wake chan struct{}

	// Used by Run alone.
	cpu            *cpuTimes
	virtualization *string
	detected       bool
	failing        bool

	mu      sync.Mutex
	pending string
}

var _ module.Reporter = (*Sampler)(nil)

func New() *Sampler {
	return &Sampler{host: newHost(), wake: make(chan struct{}, 1)}
}

func (s *Sampler) Run(ctx context.Context, publish module.Publish) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.tick(ctx, publish)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Sampler) tick(ctx context.Context, publish module.Publish) {
	data, err := s.read(ctx)
	if err != nil {
		// A file that cannot be read now usually cannot next minute either.
		if !s.failing {
			slog.Warn("metrics unreadable; samples are skipped until they read again", "error", err.Error())
		}
		s.failing = true
		return
	}
	if s.failing {
		slog.Info("metrics readable again")
		s.failing = false
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	report := protocol.ReportBody{ReportID: protocol.NewUUID(), Kind: KindSample, Data: raw}
	s.mu.Lock()
	s.pending = report.ReportID
	s.mu.Unlock()
	_ = publish(report)
}

// read takes one sample. Only the fields the protocol allows to be null may
// be missing; anything else unreadable skips the sample.
func (s *Sampler) read(ctx context.Context) (sample, error) {
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

func (s *Sampler) ControlReady() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sampler) Acknowledge(reportID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == "" || s.pending != reportID {
		return false, nil
	}
	s.pending = ""
	return true, nil
}

func (s *Sampler) UpdateSafe() error { return nil }

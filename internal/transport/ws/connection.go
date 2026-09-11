package ws

import (
	"context"
	"log/slog"
	"time"
)

const (
	connectionSetupTimeout = 30 * time.Second
	heartbeatInterval      = 30 * time.Second
	heartbeatTimeout       = 10 * time.Second
)

// watchConnection proves a round trip independently of successful socket writes.
// Closing this session wakes its reader and lets the existing reconnect loop
// recover. Operation lifetimes and durable observations belong to the runtime,
// not to this connection's watchdog.
func (s *session) watchConnection(ctx context.Context, interval, timeout time.Duration, logger *slog.Logger) func() {
	watchContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watchContext.Done():
				return
			case <-ticker.C:
				pingContext, cancelPing := context.WithTimeout(watchContext, timeout)
				err := s.connection.Ping(pingContext)
				cancelPing()
				if err != nil {
					if watchContext.Err() == nil {
						logger.Warn("control connection heartbeat failed", "code", "control_heartbeat_failed")
					}
					s.connection.CloseNow()
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

package daemon

import (
	"context"
	"errors"
	"time"
)

// The process supervisor owns startup, cancellation and trial readiness. Network
// reconciliation never delays current deployment readiness or a trial handshake.
type serviceGroup struct {
	control, maintenance func(context.Context) error
	watch                func(context.Context)
	notify               func() error
	ready                <-chan struct{}
	discardTrial         func() error
	trialTimeout         time.Duration
}

func (s serviceGroup) run(ctx context.Context) error {
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() { defer close(watchDone); s.watch(runContext) }()
	defer func() { cancel(); <-watchDone }()
	if s.discardTrial == nil {
		if err := s.notify(); err != nil {
			return err
		}
	}
	done := make(chan error, 2)
	workers := 1
	go func() {
		if s.discardTrial != nil {
			select {
			case <-s.ready:
			case <-runContext.Done():
				done <- runContext.Err()
				return
			}
		}
		done <- s.maintenance(runContext)
	}()
	if s.control != nil {
		workers++
		go func() { done <- s.control(runContext) }()
	}
	var expired <-chan time.Time
	if s.discardTrial != nil {
		timer := time.NewTimer(s.trialTimeout)
		defer timer.Stop()
		expired = timer.C
	}
	ready := s.ready
	for {
		select {
		case <-ready:
			ready, expired = nil, nil
		case <-expired:
			// Readiness may have arrived concurrently with the deadline.
			select {
			case <-s.ready:
				expired = nil
				continue
			default:
			}
			cancel()
			for range workers {
				<-done
			}
			return errors.Join(errors.New("automatic update trial did not reach control readiness"), s.discardTrial())
		case err := <-done:
			cancel()
			for range workers - 1 {
				<-done
			}
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

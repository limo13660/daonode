package task

import (
	"context"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Task struct {
	Name     string
	Interval time.Duration
	Execute  func(context.Context) error
	Access   sync.RWMutex
	Running  bool
	Stop     chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
}

func (t *Task) Start(first bool) error {
	t.Access.Lock()
	if t.Running {
		t.Access.Unlock()
		return nil
	}
	t.Running = true
	t.Stop = make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.done = make(chan struct{})
	stopCh := t.Stop
	doneCh := t.done
	t.Access.Unlock()
	go func() {
		defer close(doneCh)
		defer func() {
			t.Access.Lock()
			if t.Stop == stopCh {
				t.Running = false
				t.cancel = nil
				t.done = nil
			}
			t.Access.Unlock()
		}()
		timer := time.NewTimer(t.Interval)
		defer timer.Stop()
		if first {
			if err := t.executeWithTimeout(runCtx); err != nil {
				log.Errorf("Task %s execution error: %v", t.Name, err)
			}
		}

		for {
			timer.Reset(t.Interval)
			select {
			case <-timer.C:
				// continue
			case <-stopCh:
				return
			case <-runCtx.Done():
				return
			}

			if err := t.executeWithTimeout(runCtx); err != nil {
				log.Errorf("Task %s execution error: %v", t.Name, err)
			}
		}
	}()

	return nil
}

func (t *Task) ExecuteWithTimeout() error {
	return t.executeWithTimeout(context.Background())
}

func (t *Task) executeWithTimeout(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, min(5*t.Interval, 5*time.Minute))
	defer cancel()
	err := t.Execute(ctx)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		log.Warnf("Task %s execution timed out; keeping the current node runtime and retrying later", t.Name)
		return nil
	}
	if errors.Is(parent.Err(), context.Canceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

func (t *Task) safeStop() {
	t.Access.Lock()
	if !t.Running {
		t.Access.Unlock()
		return
	}
	t.Running = false
	stopCh := t.Stop
	cancel := t.cancel
	doneCh := t.done
	close(stopCh)
	if cancel != nil {
		cancel()
	}
	t.Access.Unlock()
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(10 * time.Second):
			log.Warnf("Task %s did not stop within 10 seconds", t.Name)
		}
	}
}

func (t *Task) Close() {
	t.safeStop()
	log.Infof("Task %s stopped", t.Name)
}

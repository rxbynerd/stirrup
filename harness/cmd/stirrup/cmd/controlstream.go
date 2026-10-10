package cmd

import (
	"sync"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
)

// watchControlStream calls onLoss with the cause if tp's control stream
// is lost before stop is called. A transport without transport.Liveness
// (stdio) is not watched. stop waits for the watcher to exit, so onLoss
// never runs after stop returns.
func watchControlStream(tp transport.Transport, onLoss func(error)) (stop func()) {
	lv, ok := tp.(transport.Liveness)
	if !ok {
		return func() {}
	}
	stopCh := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-lv.Done():
			if err := lv.Err(); err != nil {
				onLoss(err)
			}
		case <-stopCh:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stopCh) })
		<-exited
	}
}

// controlStreamErr reports why tp's control stream was lost; nil while
// it is live or when tp has no transport.Liveness.
func controlStreamErr(tp transport.Transport) error {
	if lv, ok := tp.(transport.Liveness); ok {
		return lv.Err()
	}
	return nil
}

package proxy

import (
	"testing"
	"time"
)

type reentrantStopProxy struct {
	Proxy
	callback func()
}

func (p *reentrantStopProxy) Stop() error { p.callback(); return nil }

func TestProxyShutdownDoesNotHoldRegistryLock(t *testing.T) {
	for _, all := range []bool{false, true} {
		mgr := NewManager(nil)
		mgr.proxies["broker:main"] = &runningProxy{cancel: func() {}, proxy: &reentrantStopProxy{callback: func() { mgr.GetProxyAddr("broker", "main") }}}
		done := make(chan struct{})
		go func() {
			if all {
				mgr.StopAll()
			} else {
				mgr.StopService("broker")
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("shutdown deadlocked with observer address lookup")
		}
	}
}

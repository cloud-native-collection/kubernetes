/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runner

import (
	"fmt"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

// BoundedFrequencyRunner manages runs of a user-provided work function.
// 控制函数执行频率的组件
type BoundedFrequencyRunner struct {
	name string // 实例名称

	minInterval   time.Duration // 最小间隔
	retryInterval time.Duration // 重试间隔
	maxInterval   time.Duration // 最大间隔

	run chan struct{} // 用于触发异步运行

	fn               func() error // 工作函数
	minIntervalTimer clock.Timer  // 最小间隔定时器
	nextRunTimer     clock.Timer  // Combined timer for maxInterval and retryInterval logic
	clock            clock.Clock  // 时钟
}

// NewBoundedFrequencyRunner creates and returns a new BoundedFrequencyRunner.
// This runner manages the execution frequency of the provided function `fn`.
//
// The runner guarantees two properties:
//  1. Minimum Interval (`minInterval`): At least `minInterval` must pass between
//     the *completion* of one execution and the *start* of the next. Calls to
//     `Run()` during this cooldown period are coalesced and deferred until the
//     interval expires. This prevents burst executions.
//  2. Maximum Interval (`maxInterval`): The function `fn` is guaranteed to run
//     at least once per `maxInterval`, ensuring periodic execution even without
//     explicit `Run()` calls (e.g., for refreshing state).
//
// `maxInterval` must be greater than or equal to `minInterval`; otherwise,
// this function will panic.
//
// If `fn` returns an error, then it will be run again no later than `retryInterval`
// (unless another trigger, like `Run()` or `maxInterval`, causes it to run sooner). Any
// successful run will abort the retry attempt.
func NewBoundedFrequencyRunner(name string, fn func() error, minInterval, retryInterval, maxInterval time.Duration) *BoundedFrequencyRunner {
	return construct(name, fn, minInterval, retryInterval, maxInterval, clock.RealClock{})
}

// Make an instance with dependencies injected.
func construct(name string, fn func() error, minInterval, retryInterval, maxInterval time.Duration, clock clock.Clock) *BoundedFrequencyRunner {
	if maxInterval < minInterval {
		panic(fmt.Sprintf("%s: maxInterval (%v) must be >= minInterval (%v)", name, maxInterval, minInterval))
	}

	bfr := &BoundedFrequencyRunner{
		name: name,
		fn:   fn,

		minInterval:   minInterval,
		retryInterval: retryInterval,
		maxInterval:   maxInterval,

		run:   make(chan struct{}, 1),
		clock: clock,
	}

	return bfr
}

// Loop handles the periodic timer and run requests.  This is expected to be
// called as a goroutine.
// 循环处理周期性定时器和运行请求
func (bfr *BoundedFrequencyRunner) Loop(stop <-chan struct{}) {
	klog.V(3).InfoS("Loop running", "runner", bfr.name)
	defer close(bfr.run)

	// 创建最小间隔定时器
	bfr.minIntervalTimer = bfr.clock.NewTimer(bfr.minInterval)
	defer bfr.minIntervalTimer.Stop()

	// Initialize nextRunTimer with maxInterval
	// 初始化nextRunTimer为maxInterval
	bfr.nextRunTimer = bfr.clock.NewTimer(bfr.maxInterval)
	defer bfr.nextRunTimer.Stop()

	// 循环处理周期性定时器和运行请求
	for {
		select {
		case <-stop:
			klog.V(3).InfoS("Loop stopping", "runner", bfr.name)
			return
		case <-bfr.nextRunTimer.C(): // Wait on the single timer
		case <-bfr.run: //
		}

		// stop the timers here to allow the tests using the fake clock to synchronize
		// with the fakeClock.HasWaiters() method. The timers are reset after the function
		// is executed.
		bfr.minIntervalTimer.Stop() // 停止最小间隔计时器
		bfr.nextRunTimer.Stop()     // 停止nextRunTimer

		var err error
		// avoid crashing if the function executed crashes
		func() {
			defer utilruntime.HandleCrash()
			err = bfr.fn()
		}()

		// Determine the next interval based on the result
		nextInterval := bfr.maxInterval
		if err != nil {
			// If error, ensure next run is within retryInterval and maxInterval
			if bfr.retryInterval < nextInterval {
				nextInterval = bfr.retryInterval
			}
			klog.V(3).InfoS("scheduling retry", "runner", bfr.name, "interval", nextInterval, "error", err)
		}
		// Reset the timers
		bfr.minIntervalTimer.Reset(bfr.minInterval) // 重置最小间隔计时器
		bfr.nextRunTimer.Reset(nextInterval)        // 重置nextRunTimer

		// Wait for minInterval before looping
		select {
		case <-stop:
			klog.V(3).InfoS("Loop stopping", "runner", bfr.name)
			return
		case <-bfr.minIntervalTimer.C(): // 等待minInterval
		}
	}
}

// Run the work function as soon as possible.  If this is called while Loop is not
// running, the call may be deferred indefinitely.
// Once there is a queued request to call the work function, further calls to
// Run() will have no effect until after it runs.
func (bfr *BoundedFrequencyRunner) Run() {
	// If bfr.run is empty, push an element onto it. Otherwise, do nothing.
	// 如果bfr.run为空，将一个元素推入其中。否则，什么也不做。
	select {
	case bfr.run <- struct{}{}:
	default: // // 如果已经有待处理的运行请求，则忽略
	}
}

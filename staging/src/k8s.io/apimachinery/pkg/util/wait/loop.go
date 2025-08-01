/*
Copyright 2023 The Kubernetes Authors.

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

package wait

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
)

// loopConditionUntilContext executes the provided condition at intervals defined by
// the provided timer until the provided context is cancelled, the condition returns
// true, or the condition returns an error. If sliding is true, the period is computed
// after condition runs. If it is false then period includes the runtime for condition.
// If immediate is false the first delay happens before any call to condition, if
// immediate is true the condition will be invoked before waiting and guarantees that
// the condition is invoked at least once, regardless of whether the context has been
// cancelled. The returned error is the error returned by the last condition or the
// context error if the context was terminated.
//
// This is the common loop construct for all polling in the wait package.
// 轮询模式的基础实现
func loopConditionUntilContext(ctx context.Context, t Timer, immediate, sliding bool, condition ConditionWithContextFunc) error {
	// 停止定时器
	defer t.Stop()

	// 定时器通道
	var timeCh <-chan time.Time
	// 上下文通道
	doneCh := ctx.Done()

	if !sliding {
		// 如果不是滑动模式，设置时间通道
		timeCh = t.C()
	}

	// if immediate is true the condition is
	// guaranteed to be executed at least once,
	// if we haven't requested immediate execution, delay once
	// 立即执行
	if immediate {
		if ok, err := func() (bool, error) {
			defer runtime.HandleCrashWithContext(ctx)
			return condition(ctx)
		}(); err != nil || ok {
			return err
		}
	}

	// 如果是滑动模式，设置时间通道
	if sliding {
		timeCh = t.C()
	}

	// 循环
	for {

		// Wait for either the context to be cancelled or the next invocation be called
		// 等待上下文取消或下一次调用
		select {
		case <-doneCh:
			return ctx.Err()
		case <-timeCh:
		}

		// IMPORTANT: Because there is no channel priority selection in golang
		// it is possible for very short timers to "win" the race in the previous select
		// repeatedly even when the context has been canceled.  We therefore must
		// explicitly check for context cancellation on every loop and exit if true to
		// guarantee that we don't invoke condition more than once after context has
		// been cancelled.
		// 检查上下文取消
		if err := ctx.Err(); err != nil {
			return err
		}

		// 如果不是滑动模式，更新定时器
		if !sliding {
			t.Next()
		}
		// 执行条件
		if ok, err := func() (bool, error) {
			defer runtime.HandleCrashWithContext(ctx)
			return condition(ctx)
		}(); err != nil || ok {
			return err
		}
		if sliding {
			t.Next()
		}
	}
}

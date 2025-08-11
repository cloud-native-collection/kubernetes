/*
Copyright 2016 The Kubernetes Authors.

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

package statefulset

import (
	"context"
	"sort"
	"sync"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/history"
	"k8s.io/kubernetes/pkg/features"
)

// Realistic value for maximum in-flight requests when processing in parallel mode.
const MaxBatchSize = 500

// StatefulSetControl implements the control logic for updating StatefulSets and their children Pods. It is implemented
// as an interface to allow for extensions that provide different semantics. Currently, there is only one implementation.
// StatefulSetControl 实现了更新 StatefulSet 及其子 Pod 的控制逻辑。
// 它被设计为接口，以允许提供不同语义的扩展。目前只有一个实现。
type StatefulSetControlInterface interface {
	// UpdateStatefulSet implements the control logic for Pod creation, update, and deletion, and
	// persistent volume creation, update, and deletion.
	// If an implementation returns a non-nil error, the invocation will be retried using a rate-limited strategy.
	// Implementors should sink any errors that they do not wish to trigger a retry, and they may feel free to
	// exit exceptionally at any point provided they wish the update to be re-run at a later point in time.
	// UpdateStatefulSet 实现了 Pod 的创建、更新和删除，
	// 以及持久卷的创建、更新和删除的控制逻辑。
	// 如果实现返回非 nil 错误，将使用速率限制策略重试调用。
	// 实现者应该处理他们不希望触发重试的任何错误，并且可以随时退出，
	// 只要他们希望稍后重新运行更新。
	UpdateStatefulSet(ctx context.Context, set *apps.StatefulSet, pods []*v1.Pod) (*apps.StatefulSetStatus, error)
	// ListRevisions returns a array of the ControllerRevisions that represent the revisions of set. If the returned
	// error is nil, the returns slice of ControllerRevisions is valid.
	// ListRevisions 返回表示 set 的修订版本的 ControllerRevision 数组。
	// 如果返回的错误为 nil，则返回的 ControllerRevisions 切片是有效的。
	ListRevisions(set *apps.StatefulSet) ([]*apps.ControllerRevision, error)
	// AdoptOrphanRevisions adopts any orphaned ControllerRevisions that match set's Selector. If all adoptions are
	// successful the returned error is nil.
	// AdoptOrphanRevisions 采用任何匹配 set 选择器的孤立 ControllerRevisions。如果所有采用都成功，返回的错误为 nil。
	AdoptOrphanRevisions(set *apps.StatefulSet, revisions []*apps.ControllerRevision) error
}

// NewDefaultStatefulSetControl returns a new instance of the default implementation StatefulSetControlInterface that
// implements the documented semantics for StatefulSets. podControl is the PodControlInterface used to create, update,
// and delete Pods and to create PersistentVolumeClaims. statusUpdater is the StatefulSetStatusUpdaterInterface used
// to update the status of StatefulSets. You should use an instance returned from NewRealStatefulPodControl() for any
// scenario other than testing.
func NewDefaultStatefulSetControl(
	podControl *StatefulPodControl,
	statusUpdater StatefulSetStatusUpdaterInterface,
	controllerHistory history.Interface) StatefulSetControlInterface {
	return &defaultStatefulSetControl{podControl, statusUpdater, controllerHistory}
}

// defaultStatefulSetControl 是 StatefulSet 控制逻辑的默认实现
type defaultStatefulSetControl struct {
	// podControl 用于操作 Pod 的接口
	podControl *StatefulPodControl
	// statusUpdater 用于更新 StatefulSet 状态的接口
	statusUpdater StatefulSetStatusUpdaterInterface
	// controllerHistory 用于管理 ControllerRevision 的接口
	controllerHistory history.Interface
}

// UpdateStatefulSet executes the core logic loop for a stateful set, applying the predictable and
// consistent monotonic update strategy by default - scale up proceeds in ordinal order, no new pod
// is created while any pod is unhealthy, and pods are terminated in descending order. The burst
// strategy allows these constraints to be relaxed - pods will be created and deleted eagerly and
// in no particular order. Clients using the burst strategy should be careful to ensure they
// understand the consistency implications of having unpredictable numbers of pods available.
// UpdateStatefulSet 执行 StatefulSet 的核心逻辑循环，默认应用可预测且一致的单调更新策略：
// - 按序数顺序进行扩容
// - 当任何 Pod 不健康时不会创建新 Pod
// - Pod 按降序终止
//
// 突发(Burst)策略允许放宽这些约束：
// - Pod 将被急切地创建和删除
// - 不遵循特定顺序
// 使用突发策略的客户端应小心确保他们理解 Pod 数量不可预测的可用性对一致性的影响
func (ssc *defaultStatefulSetControl) UpdateStatefulSet(ctx context.Context, set *apps.StatefulSet, pods []*v1.Pod) (*apps.StatefulSetStatus, error) {
	set = set.DeepCopy() // set is modified when a new revision is created in performUpdate. Make a copy now to avoid mutation errors.

	// list all revisions and sort them
	// 获取 StatefulSet 的所有历史修订版本
	// 对修订版本进行排序
	revisions, err := ssc.ListRevisions(set)
	if err != nil {
		return nil, err
	}
	history.SortControllerRevisions(revisions)

	// 执行实际的 StatefulSet 更新逻辑，返回当前修订版本、更新修订版本和状态
	currentRevision, updateRevision, status, err := ssc.performUpdate(ctx, set, pods, revisions)
	if err != nil {
		errs := []error{err}
		if agg, ok := err.(utilerrors.Aggregate); ok {
			errs = agg.Errors()
		}
		return nil, utilerrors.NewAggregate(append(errs, ssc.truncateHistory(set, pods, revisions, currentRevision, updateRevision)))
	}

	// maintain the set's revision history limit
	// 维护 StatefulSet 的修订历史限制，确保历史记录不超过配置的限制
	// 返回更新后的状态
	return status, ssc.truncateHistory(set, pods, revisions, currentRevision, updateRevision)
}

// 负责协调期望状态和实际状态，返回当前修订版本、更新修订版本和状态
func (ssc *defaultStatefulSetControl) performUpdate(
	ctx context.Context, set *apps.StatefulSet, pods []*v1.Pod, revisions []*apps.ControllerRevision) (*apps.ControllerRevision, *apps.ControllerRevision, *apps.StatefulSetStatus, error) {
	var currentStatus *apps.StatefulSetStatus
	logger := klog.FromContext(ctx)
	// get the current, and update revisions
	// 获取当前修订版本、更新修订版本和冲突计数
	currentRevision, updateRevision, collisionCount, err := ssc.getStatefulSetRevisions(set, revisions)
	if err != nil {
		return currentRevision, updateRevision, currentStatus, err
	}

	// perform the main update function and get the status
	// 执行主要更新函数并获取状态
	currentStatus, err = ssc.updateStatefulSet(ctx, set, currentRevision, updateRevision, collisionCount, pods)
	if err != nil && currentStatus == nil {
		return currentRevision, updateRevision, nil, err
	}

	// make sure to update the latest status even if there is an error with non-nil currentStatus
	// 更新状态：将更新后的状态写回到 API 服务器，记录详细的调试信息
	statusErr := ssc.updateStatefulSetStatus(ctx, set, currentStatus)
	if statusErr == nil {
		logger.V(4).Info("Updated status", "statefulSet", klog.KObj(set),
			"replicas", currentStatus.Replicas,
			"readyReplicas", currentStatus.ReadyReplicas,
			"currentReplicas", currentStatus.CurrentReplicas,
			"updatedReplicas", currentStatus.UpdatedReplicas)
	}

	switch {
	case err != nil && statusErr != nil:
		logger.Error(statusErr, "Could not update status", "statefulSet", klog.KObj(set))
		return currentRevision, updateRevision, currentStatus, err
	case err != nil:
		return currentRevision, updateRevision, currentStatus, err
	case statusErr != nil:
		return currentRevision, updateRevision, currentStatus, statusErr
	}

	logger.V(4).Info("StatefulSet revisions", "statefulSet", klog.KObj(set),
		"currentRevision", currentStatus.CurrentRevision,
		"updateRevision", currentStatus.UpdateRevision)

	return currentRevision, updateRevision, currentStatus, nil
}

func (ssc *defaultStatefulSetControl) ListRevisions(set *apps.StatefulSet) ([]*apps.ControllerRevision, error) {
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		return nil, err
	}
	return ssc.controllerHistory.ListControllerRevisions(set, selector)
}

func (ssc *defaultStatefulSetControl) AdoptOrphanRevisions(
	set *apps.StatefulSet,
	revisions []*apps.ControllerRevision) error {
	for i := range revisions {
		adopted, err := ssc.controllerHistory.AdoptControllerRevision(set, controllerKind, revisions[i])
		if err != nil {
			return err
		}
		revisions[i] = adopted
	}
	return nil
}

// truncateHistory truncates any non-live ControllerRevisions in revisions from set's history. The UpdateRevision and
// CurrentRevision in set's Status are considered to be live. Any revisions associated with the Pods in pods are also
// considered to be live. Non-live revisions are deleted, starting with the revision with the lowest Revision, until
// only RevisionHistoryLimit revisions remain. If the returned error is nil the operation was successful. This method
// expects that revisions is sorted when supplied.
func (ssc *defaultStatefulSetControl) truncateHistory(
	set *apps.StatefulSet,
	pods []*v1.Pod,
	revisions []*apps.ControllerRevision,
	current *apps.ControllerRevision,
	update *apps.ControllerRevision) error {
	history := make([]*apps.ControllerRevision, 0, len(revisions))
	// mark all live revisions
	live := map[string]bool{}
	if current != nil {
		live[current.Name] = true
	}
	if update != nil {
		live[update.Name] = true
	}
	for i := range pods {
		live[getPodRevision(pods[i])] = true
	}
	// collect live revisions and historic revisions
	for i := range revisions {
		if !live[revisions[i].Name] {
			history = append(history, revisions[i])
		}
	}
	historyLen := len(history)
	historyLimit := int(*set.Spec.RevisionHistoryLimit)
	if historyLimit < 0 || historyLen <= historyLimit {
		return nil
	}
	// delete any non-live history to maintain the revision limit.
	history = history[:(historyLen - historyLimit)]
	for i := 0; i < len(history); i++ {
		if err := ssc.controllerHistory.DeleteControllerRevision(history[i]); err != nil {
			return err
		}
	}
	return nil
}

// getStatefulSetRevisions returns the current and update ControllerRevisions for set. It also
// returns a collision count that records the number of name collisions set saw when creating
// new ControllerRevisions. This count is incremented on every name collision and is used in
// building the ControllerRevision names for name collision avoidance. This method may create
// a new revision, or modify the Revision of an existing revision if an update to set is detected.
// This method expects that revisions is sorted when supplied.
func (ssc *defaultStatefulSetControl) getStatefulSetRevisions(
	set *apps.StatefulSet,
	revisions []*apps.ControllerRevision) (*apps.ControllerRevision, *apps.ControllerRevision, int32, error) {
	var currentRevision, updateRevision *apps.ControllerRevision

	revisionCount := len(revisions)
	history.SortControllerRevisions(revisions)

	// Use a local copy of set.Status.CollisionCount to avoid modifying set.Status directly.
	// This copy is returned so the value gets carried over to set.Status in updateStatefulSet.
	var collisionCount int32
	if set.Status.CollisionCount != nil {
		collisionCount = *set.Status.CollisionCount
	}

	// create a new revision from the current set
	updateRevision, err := newRevision(set, nextRevision(revisions), &collisionCount)
	if err != nil {
		return nil, nil, collisionCount, err
	}

	// find any equivalent revisions
	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)

	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		updateRevision, err = ssc.controllerHistory.UpdateControllerRevision(
			equalRevisions[equalCount-1],
			updateRevision.Revision)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = ssc.controllerHistory.CreateControllerRevision(set, updateRevision, &collisionCount)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == set.Status.CurrentRevision {
			currentRevision = revisions[i]
			break
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, collisionCount, nil
}

func slowStartBatch(initialBatchSize int, remaining int, fn func(int) (bool, error)) (int, error) {
	successes := 0
	j := 0
	for batchSize := min(remaining, initialBatchSize); batchSize > 0; batchSize = min(min(2*batchSize, remaining), MaxBatchSize) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func(k int) {
				defer wg.Done()
				// Ignore the first parameter - relevant for monotonic only.
				if _, err := fn(k); err != nil {
					errCh <- err
				}
			}(j)
			j++
		}
		wg.Wait()
		successes += batchSize - len(errCh)
		close(errCh)
		if len(errCh) > 0 {
			errs := make([]error, 0)
			for err := range errCh {
				errs = append(errs, err)
			}
			return successes, utilerrors.NewAggregate(errs)
		}
		remaining -= batchSize
	}
	return successes, nil
}

type replicaStatus struct {
	replicas          int32
	readyReplicas     int32
	availableReplicas int32
	currentReplicas   int32
	updatedReplicas   int32
}

func computeReplicaStatus(pods []*v1.Pod, minReadySeconds int32, currentRevision, updateRevision *apps.ControllerRevision) replicaStatus {
	status := replicaStatus{}
	for _, pod := range pods {
		if isCreated(pod) {
			status.replicas++
		}

		// count the number of running and ready replicas
		if isRunningAndReady(pod) {
			status.readyReplicas++
			// count the number of running and available replicas
			if isRunningAndAvailable(pod, minReadySeconds) {
				status.availableReplicas++
			}

		}

		// count the number of current and update replicas
		if isCreated(pod) && !isTerminating(pod) {
			revision := getPodRevision(pod)
			if revision == currentRevision.Name {
				status.currentReplicas++
			}
			if revision == updateRevision.Name {
				status.updatedReplicas++
			}
		}
	}
	return status
}

func updateStatus(status *apps.StatefulSetStatus, minReadySeconds int32, currentRevision, updateRevision *apps.ControllerRevision, podLists ...[]*v1.Pod) {
	status.Replicas = 0
	status.ReadyReplicas = 0
	status.AvailableReplicas = 0
	status.CurrentReplicas = 0
	status.UpdatedReplicas = 0
	for _, list := range podLists {
		replicaStatus := computeReplicaStatus(list, minReadySeconds, currentRevision, updateRevision)
		status.Replicas += replicaStatus.replicas
		status.ReadyReplicas += replicaStatus.readyReplicas
		status.AvailableReplicas += replicaStatus.availableReplicas
		status.CurrentReplicas += replicaStatus.currentReplicas
		status.UpdatedReplicas += replicaStatus.updatedReplicas
	}
}

func (ssc *defaultStatefulSetControl) processReplica(
	ctx context.Context,
	set *apps.StatefulSet,
	updateSet *apps.StatefulSet,
	monotonic bool,
	replicas []*v1.Pod,
	i int) (bool, error) {
	logger := klog.FromContext(ctx)

	// Note that pods with phase Succeeded will also trigger this event. This is
	// because final pod phase of evicted or otherwise forcibly stopped pods
	// (e.g. terminated on node reboot) is determined by the exit code of the
	// container, not by the reason for pod termination. We should restart the pod
	// regardless of the exit code.
	if isFailed(replicas[i]) || isSucceeded(replicas[i]) {
		if replicas[i].DeletionTimestamp == nil {
			if err := ssc.podControl.DeleteStatefulPod(set, replicas[i]); err != nil {
				return true, err
			}
		}
		// New pod should be generated on the next sync after the current pod is removed from etcd.
		return true, nil
	}
	// If we find a Pod that has not been created we create the Pod
	if !isCreated(replicas[i]) {
		if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
			if isStale, err := ssc.podControl.PodClaimIsStale(set, replicas[i]); err != nil {
				return true, err
			} else if isStale {
				// If a pod has a stale PVC, no more work can be done this round.
				return true, err
			}
		}
		if err := ssc.podControl.CreateStatefulPod(ctx, set, replicas[i]); err != nil {
			return true, err
		}
		if monotonic {
			// if the set does not allow bursting, return immediately
			return true, nil
		}
	}

	// If the Pod is in pending state then trigger PVC creation to create missing PVCs
	if isPending(replicas[i]) {
		logger.V(4).Info(
			"StatefulSet is triggering PVC creation for pending Pod",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[i]))
		if err := ssc.podControl.createMissingPersistentVolumeClaims(ctx, set, replicas[i]); err != nil {
			return true, err
		}
	}

	// If we find a Pod that is currently terminating, we must wait until graceful deletion
	// completes before we continue to make progress.
	if isTerminating(replicas[i]) && monotonic {
		logger.V(4).Info("StatefulSet is waiting for Pod to Terminate",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[i]))
		return true, nil
	}

	// If we have a Pod that has been created but is not running and ready we can not make progress.
	// We must ensure that all for each Pod, when we create it, all of its predecessors, with respect to its
	// ordinal, are Running and Ready.
	if !isRunningAndReady(replicas[i]) && monotonic {
		logger.V(4).Info("StatefulSet is waiting for Pod to be Running and Ready",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[i]))
		return true, nil
	}

	// If we have a Pod that has been created but is not available we can not make progress.
	// We must ensure that all for each Pod, when we create it, all of its predecessors, with respect to its
	// ordinal, are Available.
	if !isRunningAndAvailable(replicas[i], set.Spec.MinReadySeconds) && monotonic {
		logger.V(4).Info("StatefulSet is waiting for Pod to be Available",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[i]))
		return true, nil
	}

	// Enforce the StatefulSet invariants
	retentionMatch := true
	if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
		var err error
		retentionMatch, err = ssc.podControl.ClaimsMatchRetentionPolicy(ctx, updateSet, replicas[i])
		// An error is expected if the pod is not yet fully updated, and so return is treated as matching.
		if err != nil {
			retentionMatch = true
		}
	}

	if identityMatches(set, replicas[i]) && storageMatches(set, replicas[i]) && retentionMatch {
		return false, nil
	}

	// Make a deep copy so we don't mutate the shared cache
	replica := replicas[i].DeepCopy()
	if err := ssc.podControl.UpdateStatefulPod(ctx, updateSet, replica); err != nil {
		return true, err
	}

	return false, nil
}

// ⭐️ processCondemned 顺序终止 Pod，处理需要被终止的 Pod（即超出期望副本数的 Pod），确保它们按照正确的顺序和条件被删除
func (ssc *defaultStatefulSetControl) processCondemned(ctx context.Context, set *apps.StatefulSet, firstUnhealthyPod *v1.Pod, monotonic bool, condemned []*v1.Pod, i int) (bool, error) {
	logger := klog.FromContext(ctx)
	// 如果 condemned[i] 正在终止中，返回 true，等待终止完成
	if isTerminating(condemned[i]) {
		// if we are in monotonic mode, block and wait for terminating pods to expire
		// 如果是单调模式，阻塞并等待终止完成
		if monotonic {
			logger.V(4).Info("StatefulSet is waiting for Pod to Terminate prior to scale down",
				"statefulSet", klog.KObj(set), "pod", klog.KObj(condemned[i]))
			return true, nil
		}
		return false, nil
	}
	// if we are in monotonic mode and the condemned target is not the first unhealthy Pod block
	// 如果是单调模式，且 condemned[i] 不是第一个不健康 Pod，阻塞
	if !isRunningAndReady(condemned[i]) && monotonic && condemned[i] != firstUnhealthyPod {
		logger.V(4).Info("StatefulSet is waiting for Pod to be Running and Ready prior to scale down",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(firstUnhealthyPod))
		return true, nil
	}
	// if we are in monotonic mode and the condemned target is not the first unhealthy Pod, block.
	// 如果是单调模式，且 condemned[i] 不是第一个不健康 Pod，阻塞，等待 Pod 可用
	if !isRunningAndAvailable(condemned[i], set.Spec.MinReadySeconds) && monotonic && condemned[i] != firstUnhealthyPod {
		logger.V(4).Info("StatefulSet is waiting for Pod to be Available prior to scale down",
			"statefulSet", klog.KObj(set), "pod", klog.KObj(firstUnhealthyPod))
		return true, nil
	}

	logger.V(2).Info("Pod of StatefulSet is terminating for scale down",
		"statefulSet", klog.KObj(set), "pod", klog.KObj(condemned[i]))
	// 删除 Pod
	return true, ssc.podControl.DeleteStatefulPod(set, condemned[i])
}

func runForAll(pods []*v1.Pod, fn func(i int) (bool, error), monotonic bool) (bool, error) {
	if monotonic {
		for i := range pods {
			if shouldExit, err := fn(i); shouldExit || err != nil {
				return true, err
			}
		}
	} else {
		if _, err := slowStartBatch(1, len(pods), fn); err != nil {
			return true, err
		}
	}
	return false, nil
}

// updateStatefulSet performs the update function for a StatefulSet. This method creates, updates, and deletes Pods in
// the set in order to conform the system to the target state for the set. The target state always contains
// set.Spec.Replicas Pods with a Ready Condition. If the UpdateStrategy.Type for the set is
// RollingUpdateStatefulSetStrategyType then all Pods in the set must be at set.Status.CurrentRevision.
// If the UpdateStrategy.Type for the set is OnDeleteStatefulSetStrategyType, the target state implies nothing about
// the revisions of Pods in the set. If the UpdateStrategy.Type for the set is PartitionStatefulSetStrategyType, then
// all Pods with ordinal less than UpdateStrategy.Partition.Ordinal must be at Status.CurrentRevision and all other
// Pods must be at Status.UpdateRevision. If the returned error is nil, the returned StatefulSetStatus is valid and the
// update must be recorded. If the error is not nil, the method should be retried until successful.
// updateStatefulSet 执行 StatefulSet 的更新功能。此方法创建、更新和删除 Pod，
// 以使系统符合集合的目标状态。目标状态始终包含 set.Spec.Replicas 个具有 Ready 条件的 Pod。
// 如果集合的 UpdateStrategy.Type 是 RollingUpdateStatefulSetStrategyType，
// 则集合中的所有 Pod 必须处于 set.Status.CurrentRevision 版本。
// 如果集合的 UpdateStrategy.Type 是 OnDeleteStatefulSetStrategyType，
// 则目标状态不关心 Pod 的版本。
// 如果集合的 UpdateStrategy.Type 是 PartitionStatefulSetStrategyType，
// 则序号小于 UpdateStrategy.Partition.Ordinal 的所有 Pod 必须处于 Status.CurrentRevision 版本，
// 其他所有 Pod 必须处于 Status.UpdateRevision 版本。
// 如果返回的错误为 nil，则返回的 StatefulSetStatus 有效且必须记录更新。
// 如果错误不为 nil，则应重试此方法直到成功。
// ⭐️ updateStatefulSet 执行 StatefulSet 的更新功能，确保顺序创建和删除 Pod。
func (ssc *defaultStatefulSetControl) updateStatefulSet(
	ctx context.Context,
	set *apps.StatefulSet,
	currentRevision *apps.ControllerRevision,
	updateRevision *apps.ControllerRevision,
	collisionCount int32,
	pods []*v1.Pod) (*apps.StatefulSetStatus, error) {
	logger := klog.FromContext(ctx)
	// get the current and update revisions of the set.
	// 获取当前和更新修订版本
	currentSet, err := ApplyRevision(set, currentRevision)
	if err != nil {
		return nil, err
	}
	updateSet, err := ApplyRevision(set, updateRevision)
	if err != nil {
		return nil, err
	}

	// set the generation, and revisions in the returned status
	// 设置返回状态的 generation 和修订版本
	status := apps.StatefulSetStatus{}
	status.ObservedGeneration = set.Generation
	status.CurrentRevision = currentRevision.Name
	status.UpdateRevision = updateRevision.Name
	status.CollisionCount = new(int32)
	*status.CollisionCount = collisionCount

	updateStatus(&status, set.Spec.MinReadySeconds, currentRevision, updateRevision, pods)

	replicaCount := int(*set.Spec.Replicas)
	// slice that will contain all Pods such that getStartOrdinal(set) <= getOrdinal(pod) <= getEndOrdinal(set)
	//  在期望副本数范围内的 Pod
	replicas := make([]*v1.Pod, replicaCount)
	// slice that will contain all Pods such that getOrdinal(pod) < getStartOrdinal(set) OR getOrdinal(pod) > getEndOrdinal(set)
	//  需要被终止的 Pod（超出副本数范围）
	condemned := make([]*v1.Pod, 0, len(pods))
	unavailable := 0
	var firstUnavailablePod *v1.Pod

	// First we partition pods into two lists valid replicas and condemned Pods
	// 将 Pod 分成两个列表：有效的副本和被谴责的 Pod
	for _, pod := range pods {
		// 如果 Pod 的 ordinal 在期望副本数范围内
		if podInOrdinalRange(pod, set) {
			// if the ordinal of the pod is within the range of the current number of replicas,
			// insert it at the indirection of its ordinal
			// 将 Pod 放入 replicas 列表中
			replicas[getOrdinal(pod)-getStartOrdinal(set)] = pod
			// 如果 Pod 的 ordinal 在期望副本数范围内，但不在当前副本数范围内，将 Pod 放入 condemned 列表中
		} else if getOrdinal(pod) >= 0 {
			// if the ordinal is valid, but not within the range add it to the condemned list
			condemned = append(condemned, pod)
		}
		// If the ordinal could not be parsed (ord < 0), ignore the Pod.
	}

	// for any empty indices in the sequence [0,set.Spec.Replicas) create a new Pod at the correct revision
	// 为序列 [0,set.Spec.Replicas) 中的任何空索引创建一个新 Pod
	start, end := getStartOrdinal(set), getEndOrdinal(set)
	for ord := start; ord <= end; ord++ {
		replicaIdx := ord - start
		if replicas[replicaIdx] == nil {
			replicas[replicaIdx] = newVersionedStatefulSetPod(
				currentSet,
				updateSet,
				currentRevision.Name,
				updateRevision.Name, ord)
		}
	}

	// sort the condemned Pods by their ordinals
	sort.Sort(descendingOrdinal(condemned))

	// find the first unhealthy Pod
	// 检查不可用 Pod 数量，记录第一个不可用 Pod 用于日志记录
	for i := range replicas {
		if isUnavailable(replicas[i], set.Spec.MinReadySeconds) {
			unavailable++
			if firstUnavailablePod == nil {
				firstUnavailablePod = replicas[i]
			}
		}
	}

	// or the first unhealthy condemned Pod (condemned are sorted in descending order for ease of use)
	for i := len(condemned) - 1; i >= 0; i-- {
		if isUnavailable(condemned[i], set.Spec.MinReadySeconds) {
			unavailable++
			if firstUnavailablePod == nil {
				firstUnavailablePod = condemned[i]
			}
		}
	}

	if unavailable > 0 {
		logger.V(4).Info("StatefulSet has unavailable Pods", "statefulSet", klog.KObj(set), "unavailableReplicas", unavailable, "pod", klog.KObj(firstUnavailablePod))
	}

	// If the StatefulSet is being deleted, don't do anything other than updating
	// status.
	if set.DeletionTimestamp != nil {
		return &status, nil
	}

	monotonic := !allowsBurst(set)

	// First, process each living replica. Exit if we run into an error or something blocking in monotonic mode.
	// 首先，处理每个活着的副本。如果在单调模式下遇到错误或阻塞，则退出。
	processReplicaFn := func(i int) (bool, error) {
		return ssc.processReplica(ctx, set, updateSet, monotonic, replicas, i)
	}
	if shouldExit, err := runForAll(replicas, processReplicaFn, monotonic); shouldExit || err != nil {
		updateStatus(&status, set.Spec.MinReadySeconds, currentRevision, updateRevision, replicas, condemned)
		return &status, err
	}

	// Fix pod claims for condemned pods, if necessary.
	// 如果需要，修复 condemned Pod 的 claims
	if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
		fixPodClaim := func(i int) (bool, error) {
			if matchPolicy, err := ssc.podControl.ClaimsMatchRetentionPolicy(ctx, updateSet, condemned[i]); err != nil {
				return true, err
			} else if !matchPolicy {
				if err := ssc.podControl.UpdatePodClaimForRetentionPolicy(ctx, updateSet, condemned[i]); err != nil {
					return true, err
				}
			}
			return false, nil
		}
		if shouldExit, err := runForAll(condemned, fixPodClaim, monotonic); shouldExit || err != nil {
			updateStatus(&status, set.Spec.MinReadySeconds, currentRevision, updateRevision, replicas, condemned)
			return &status, err
		}
	}

	// At this point, in monotonic mode all of the current Replicas are Running, Ready and Available,
	// and we can consider termination.
	// We will wait for all predecessors to be Running and Available prior to attempting a deletion.
	// We will terminate Pods in a monotonically decreasing order.
	// Note that we do not resurrect Pods in this interval. Also note that scaling will take precedence over
	// updates.
	// 处理待终止 Pod
	processCondemnedFn := func(i int) (bool, error) {
		return ssc.processCondemned(ctx, set, firstUnavailablePod, monotonic, condemned, i)
	}
	if shouldExit, err := runForAll(condemned, processCondemnedFn, monotonic); shouldExit || err != nil {
		updateStatus(&status, set.Spec.MinReadySeconds, currentRevision, updateRevision, replicas, condemned)
		return &status, err
	}

	updateStatus(&status, set.Spec.MinReadySeconds, currentRevision, updateRevision, replicas, condemned)

	// for the OnDelete strategy we short circuit. Pods will be updated when they are manually deleted.
	// 更新策略处理：支持不同的更新策略，OnDelete 策略直接返回，等待手动删除 Pod
	if set.Spec.UpdateStrategy.Type == apps.OnDeleteStatefulSetStrategyType {
		return &status, nil
	}

	// 如果启用了 MaxUnavailableStatefulSet，使用 updateStatefulSetAfterInvariantEstablished 更新 StatefulSet
	if utilfeature.DefaultFeatureGate.Enabled(features.MaxUnavailableStatefulSet) {
		// 使用 updateStatefulSetAfterInvariantEstablished 更新 StatefulSet
		return updateStatefulSetAfterInvariantEstablished(ctx,
			ssc,
			set,
			replicas,
			updateRevision,
			status,
		)
	}

	// we compute the minimum ordinal of the target sequence for a destructive update based on the strategy.
	updateMin := 0
	if set.Spec.UpdateStrategy.RollingUpdate != nil {
		updateMin = int(*set.Spec.UpdateStrategy.RollingUpdate.Partition)
	}
	// we terminate the Pod with the largest ordinal that does not match the update revision.
	// 滚动更新，从高序号到低序号更新 Pod，确保更新顺序
	for target := len(replicas) - 1; target >= updateMin; target-- {

		// delete the Pod if it is not already terminating and does not match the update revision.
		// 如果 Pod 的版本不是目标版本且不在终止过程中，则删除该 Pod
		if getPodRevision(replicas[target]) != updateRevision.Name && !isTerminating(replicas[target]) {
			logger.V(2).Info("Pod of StatefulSet is terminating for update",
				"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[target]))
			// 删除 Pod
			if err := ssc.podControl.DeleteStatefulPod(set, replicas[target]); err != nil {
				if !errors.IsNotFound(err) {
					return &status, err
				}
			}
			status.CurrentReplicas--
			return &status, err
		}

		// wait for unavailable Pods on update
		// 等待不可用的 Pod 更新
		if isUnavailable(replicas[target], set.Spec.MinReadySeconds) {
			logger.V(4).Info("StatefulSet is waiting for Pod to update",
				"statefulSet", klog.KObj(set), "pod", klog.KObj(replicas[target]))
			return &status, nil
		}

	}
	return &status, nil
}

// ⭐️ 实现 StatefulSet 滚动更新
func updateStatefulSetAfterInvariantEstablished(
	ctx context.Context,
	ssc *defaultStatefulSetControl,
	set *apps.StatefulSet,
	replicas []*v1.Pod,
	updateRevision *apps.ControllerRevision,
	status apps.StatefulSetStatus,
) (*apps.StatefulSetStatus, error) {

	logger := klog.FromContext(ctx)
	replicaCount := int(*set.Spec.Replicas)

	// we compute the minimum ordinal of the target sequence for a destructive update based on the strategy.
	updateMin := 0
	maxUnavailable := 1
	// 计算滚动更新的最小序号
	if set.Spec.UpdateStrategy.RollingUpdate != nil {
		updateMin = int(*set.Spec.UpdateStrategy.RollingUpdate.Partition)

		// if the feature was enabled and then later disabled, MaxUnavailable may have a value
		// more than 1. Ignore the passed in value and Use maxUnavailable as 1 to enforce
		// expected behavior when feature gate is not enabled.
		var err error
		maxUnavailable, err = getStatefulSetMaxUnavailable(set.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable, replicaCount)
		if err != nil {
			return &status, err
		}
	}

	// Collect all targets in the range between getStartOrdinal(set) and getEndOrdinal(set). Count any targets in that range
	// that are unavailable. Select the
	// (MaxUnavailable - Unavailable) Pods, in order with respect to their ordinal for termination. Delete
	// those pods and count the successful deletions. Update the status with the correct number of deletions.
	// 计算不可用的 Pod 数量
	unavailablePods := 0
	for target := len(replicas) - 1; target >= 0; target-- {
		if isUnavailable(replicas[target], set.Spec.MinReadySeconds) {
			unavailablePods++
		}
	}

	// 如果不可用的 Pod 数量大于等于 maxUnavailable，直接返回
	if unavailablePods >= maxUnavailable {
		logger.V(2).Info("StatefulSet found unavailablePods, more than or equal to allowed maxUnavailable",
			"statefulSet", klog.KObj(set),
			"unavailablePods", unavailablePods,
			"maxUnavailable", maxUnavailable)
		return &status, nil
	}

	// Now we need to delete MaxUnavailable- unavailablePods
	// start deleting one by one starting from the highest ordinal first
	// 计算需要删除的 Pod 数量
	podsToDelete := maxUnavailable - unavailablePods

	// 计算删除的 Pod 数量
	deletedPods := 0
	for target := len(replicas) - 1; target >= updateMin && deletedPods < podsToDelete; target-- {

		// delete the Pod if it is healthy and the revision doesnt match the target
		if getPodRevision(replicas[target]) != updateRevision.Name && !isTerminating(replicas[target]) {
			// delete the Pod if it is healthy and the revision doesnt match the target
			logger.V(2).Info("StatefulSet terminating Pod for update",
				"statefulSet", klog.KObj(set),
				"pod", klog.KObj(replicas[target]))
			if err := ssc.podControl.DeleteStatefulPod(set, replicas[target]); err != nil {
				if !errors.IsNotFound(err) {
					return &status, err
				}
			}
			deletedPods++
			status.CurrentReplicas--
		}
	}
	return &status, nil
}

// updateStatefulSetStatus updates set's Status to be equal to status. If status indicates a complete update, it is
// mutated to indicate completion. If status is semantically equivalent to set's Status no update is performed. If the
// returned error is nil, the update is successful.
func (ssc *defaultStatefulSetControl) updateStatefulSetStatus(
	ctx context.Context,
	set *apps.StatefulSet,
	status *apps.StatefulSetStatus) error {
	// complete any in progress rolling update if necessary
	completeRollingUpdate(set, status)

	// if the status is not inconsistent do not perform an update
	if !inconsistentStatus(set, status) {
		return nil
	}

	// copy set and update its status
	set = set.DeepCopy()
	if err := ssc.statusUpdater.UpdateStatefulSetStatus(ctx, set, status); err != nil {
		return err
	}

	return nil
}

var _ StatefulSetControlInterface = &defaultStatefulSetControl{}

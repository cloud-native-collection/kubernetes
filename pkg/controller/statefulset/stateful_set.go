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
	"fmt"
	"reflect"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/history"

	"k8s.io/klog/v2"
)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = apps.SchemeGroupVersion.WithKind("StatefulSet")

// podKind contains the schema.GroupVersionKind for pods.
var podKind = v1.SchemeGroupVersion.WithKind("Pod")

// StatefulSetController controls statefulsets.
// statefulSet 控制器的核心结构
type StatefulSetController struct {
	// client interface
	// kubeClient 是 Kubernetes API 的客户端接口，用于与 Kubernetes API 交互
	kubeClient clientset.Interface
	// control returns an interface capable of syncing a stateful set.
	// Abstracted out for testing.
	// 控制接口，抽象了 StatefulSet 的同步逻辑,主要用于测试时注入模拟实现
	control StatefulSetControlInterface
	// podControl is used for patching pods.
	// podControl 用于操作 Pod 的接口
	podControl controller.PodControlInterface
	// podIndexer allows looking up pods by ControllerRef UID
	// Pod 索引器，支持通过 ControllerRef UID 查找 Pod
	podIndexer cache.Indexer
	// podLister is able to list/get pods from a shared informer's store
	// Pod 列表器，用于从共享 informer 的存储中列出/获取 Pod
	podLister corelisters.PodLister
	// podListerSynced returns true if the pod shared informer has synced at least once
	// podListerSynced 返回 true 如果 pod 共享 informer 已至少同步一次
	podListerSynced cache.InformerSynced
	// setLister is able to list/get stateful sets from a shared informer's store
	// setLister 是能够从共享 informer 的存储中列出/获取 stateful sets 的接口
	setLister appslisters.StatefulSetLister
	// setListerSynced returns true if the stateful set shared informer has synced at least once
	// setListerSynced 返回 true 如果 stateful set 共享 informer 已至少同步一次
	setListerSynced cache.InformerSynced
	// pvcListerSynced returns true if the pvc shared informer has synced at least once
	// pvcListerSynced 返回 true 如果 pvc 共享 informer 已至少同步一次
	pvcListerSynced cache.InformerSynced
	// revListerSynced returns true if the rev shared informer has synced at least once
	// revListerSynced 返回 true 如果 rev 共享 informer 已至少同步一次
	revListerSynced cache.InformerSynced
	// StatefulSets that need to be synced.
	// 需要同步的 StatefulSets
	queue workqueue.TypedRateLimitingInterface[string]
	// eventBroadcaster is the core of event processing pipeline.
	// 事件广播器，用于处理事件
	eventBroadcaster record.EventBroadcaster
}

// NewStatefulSetController creates a new statefulset controller.
func NewStatefulSetController(
	ctx context.Context,
	podInformer coreinformers.PodInformer,
	setInformer appsinformers.StatefulSetInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	revInformer appsinformers.ControllerRevisionInformer,
	kubeClient clientset.Interface,
) *StatefulSetController {
	logger := klog.FromContext(ctx)
	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "statefulset-controller"})
	ssc := &StatefulSetController{
		kubeClient: kubeClient,
		control: NewDefaultStatefulSetControl(
			NewStatefulPodControl(
				kubeClient,
				podInformer.Lister(),
				pvcInformer.Lister(),
				recorder),
			NewRealStatefulSetStatusUpdater(kubeClient, setInformer.Lister()),
			history.NewHistory(kubeClient, revInformer.Lister()),
		),
		pvcListerSynced: pvcInformer.Informer().HasSynced,
		revListerSynced: revInformer.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "statefulset"},
		),
		podControl: controller.RealPodControl{KubeClient: kubeClient, Recorder: recorder},

		eventBroadcaster: eventBroadcaster,
	}

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		// lookup the statefulset and enqueue
		AddFunc: func(obj interface{}) {
			ssc.addPod(logger, obj)
		},
		// lookup current and old statefulset if labels changed
		UpdateFunc: func(oldObj, newObj interface{}) {
			ssc.updatePod(logger, oldObj, newObj)
		},
		// lookup statefulset accounting for deletion tombstones
		DeleteFunc: func(obj interface{}) {
			ssc.deletePod(logger, obj)
		},
	})
	ssc.podLister = podInformer.Lister()
	ssc.podListerSynced = podInformer.Informer().HasSynced
	controller.AddPodControllerUIDIndexer(podInformer.Informer())
	ssc.podIndexer = podInformer.Informer().GetIndexer()
	setInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: ssc.enqueueStatefulSet,
			UpdateFunc: func(old, cur interface{}) {
				oldPS := old.(*apps.StatefulSet)
				curPS := cur.(*apps.StatefulSet)
				if oldPS.Status.Replicas != curPS.Status.Replicas {
					logger.V(4).Info("Observed updated replica count for StatefulSet", "statefulSet", klog.KObj(curPS), "oldReplicas", oldPS.Status.Replicas, "newReplicas", curPS.Status.Replicas)
				}
				ssc.enqueueStatefulSet(cur)
			},
			DeleteFunc: ssc.enqueueStatefulSet,
		},
	)
	ssc.setLister = setInformer.Lister()
	ssc.setListerSynced = setInformer.Informer().HasSynced

	// TODO: Watch volumes
	return ssc
}

// Run runs the statefulset controller.
// 控制器的入口，负责启动和运行控制器。
func (ssc *StatefulSetController) Run(ctx context.Context, workers int) {
	// 1. 确保在 panic 时能够恢复
	defer utilruntime.HandleCrash()

	// Start events processing pipeline.
	// 2. 启动事件处理管道
	// 2.1 启动结构化日志记录
	ssc.eventBroadcaster.StartStructuredLogging(3)
	// 2.2 启动事件记录到 API Server
	ssc.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: ssc.kubeClient.CoreV1().Events("")})
	// 2.3 确保在退出时关闭事件广播器
	defer ssc.eventBroadcaster.Shutdown()

	// 3. 停止事件处理管道
	defer ssc.queue.ShutDown()

	// 4. 设置日志记录器
	logger := klog.FromContext(ctx)
	logger.Info("Starting stateful set controller")
	defer logger.Info("Shutting down statefulset controller")

	// 5. 等待缓存同步完成
	if !cache.WaitForNamedCacheSync("stateful set", ctx.Done(), ssc.podListerSynced, ssc.setListerSynced, ssc.pvcListerSynced, ssc.revListerSynced) {
		return
	}

	// 6. 启动指定数量的工作协程
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, ssc.worker, time.Second)
	}

	<-ctx.Done()
}

// addPod adds the statefulset for the pod to the sync queue
func (ssc *StatefulSetController) addPod(logger klog.Logger, obj interface{}) {
	pod := obj.(*v1.Pod)

	if pod.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new pod shows up in a state that
		// is already pending deletion. Prevent the pod from being a creation observation.
		ssc.deletePod(logger, pod)
		return
	}

	// If it has a ControllerRef, that's all that matters.
	if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil {
		set := ssc.resolveControllerRef(pod.Namespace, controllerRef)
		if set == nil {
			return
		}
		logger.V(4).Info("Pod created with labels", "pod", klog.KObj(pod), "labels", pod.Labels)
		ssc.enqueueStatefulSet(set)
		return
	}

	// Otherwise, it's an orphan. Get a list of all matching controllers and sync
	// them to see if anyone wants to adopt it.
	sets := ssc.getStatefulSetsForPod(pod)
	if len(sets) == 0 {
		return
	}
	logger.V(4).Info("Orphan Pod created with labels", "pod", klog.KObj(pod), "labels", pod.Labels)
	for _, set := range sets {
		ssc.enqueueStatefulSet(set)
	}
}

// updatePod adds the statefulset for the current and old pods to the sync queue.
func (ssc *StatefulSetController) updatePod(logger klog.Logger, old, cur interface{}) {
	curPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// In the event of a re-list we may receive update events for all known pods.
		// Two different versions of the same pod will always have different RVs.
		return
	}

	labelChanged := !reflect.DeepEqual(curPod.Labels, oldPod.Labels)

	curControllerRef := metav1.GetControllerOf(curPod)
	oldControllerRef := metav1.GetControllerOf(oldPod)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if set := ssc.resolveControllerRef(oldPod.Namespace, oldControllerRef); set != nil {
			ssc.enqueueStatefulSet(set)
		}
	}

	// If it has a ControllerRef, that's all that matters.
	if curControllerRef != nil {
		set := ssc.resolveControllerRef(curPod.Namespace, curControllerRef)
		if set == nil {
			return
		}
		logger.V(4).Info("Pod objectMeta updated", "pod", klog.KObj(curPod), "oldObjectMeta", oldPod.ObjectMeta, "newObjectMeta", curPod.ObjectMeta)
		if oldPod.Status.Phase != curPod.Status.Phase {
			logger.V(4).Info("StatefulSet Pod phase changed", "pod", klog.KObj(curPod), "statefulSet", klog.KObj(set), "podPhase", curPod.Status.Phase)
		}
		ssc.enqueueStatefulSet(set)
		// TODO: MinReadySeconds in the Pod will generate an Available condition to be added in
		// the Pod status which in turn will trigger a requeue of the owning replica set thus
		// having its status updated with the newly available replica.
		if !podutil.IsPodReady(oldPod) && podutil.IsPodReady(curPod) && set.Spec.MinReadySeconds > 0 {
			logger.V(2).Info("StatefulSet will be enqueued after minReadySeconds for availability check", "statefulSet", klog.KObj(set), "minReadySeconds", set.Spec.MinReadySeconds)
			// Add a second to avoid milliseconds skew in AddAfter.
			// See https://github.com/kubernetes/kubernetes/issues/39785#issuecomment-279959133 for more info.
			ssc.enqueueSSAfter(set, (time.Duration(set.Spec.MinReadySeconds)*time.Second)+time.Second)
		}
		return
	}

	// Otherwise, it's an orphan. If anything changed, sync matching controllers
	// to see if anyone wants to adopt it now.
	if labelChanged || controllerRefChanged {
		sets := ssc.getStatefulSetsForPod(curPod)
		if len(sets) == 0 {
			return
		}
		logger.V(4).Info("Orphan Pod objectMeta updated", "pod", klog.KObj(curPod), "oldObjectMeta", oldPod.ObjectMeta, "newObjectMeta", curPod.ObjectMeta)
		for _, set := range sets {
			ssc.enqueueStatefulSet(set)
		}
	}
}

// deletePod enqueues the statefulset for the pod accounting for deletion tombstones.
func (ssc *StatefulSetController) deletePod(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %+v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %+v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	set := ssc.resolveControllerRef(pod.Namespace, controllerRef)
	if set == nil {
		return
	}
	logger.V(4).Info("Pod deleted.", "pod", klog.KObj(pod), "caller", utilruntime.GetCaller())
	ssc.enqueueStatefulSet(set)
}

// getPodsForStatefulSet returns the Pods that a given StatefulSet should manage.
// It also reconciles ControllerRef by adopting/orphaning.
//
// NOTE: Returned Pods are pointers to objects from the cache.
// If you need to modify one, you need to copy it first.
func (ssc *StatefulSetController) getPodsForStatefulSet(ctx context.Context, set *apps.StatefulSet, selector labels.Selector) ([]*v1.Pod, error) {
	podsForSts, err := controller.FilterPodsByOwner(ssc.podIndexer, &set.ObjectMeta)
	if err != nil {
		return nil, err
	}

	filter := func(pod *v1.Pod) bool {
		// Only claim if it matches our StatefulSet name. Otherwise release/ignore.
		return isMemberOf(set, pod)
	}

	cm := controller.NewPodControllerRefManager(ssc.podControl, set, selector, controllerKind, ssc.canAdoptFunc(ctx, set))
	return cm.ClaimPods(ctx, podsForSts, filter)
}

// If any adoptions are attempted, we should first recheck for deletion with
// an uncached quorum read sometime after listing Pods/ControllerRevisions (see #42639).
func (ssc *StatefulSetController) canAdoptFunc(ctx context.Context, set *apps.StatefulSet) func(ctx2 context.Context) error {
	return controller.RecheckDeletionTimestamp(func(ctx context.Context) (metav1.Object, error) {
		fresh, err := ssc.kubeClient.AppsV1().StatefulSets(set.Namespace).Get(ctx, set.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != set.UID {
			return nil, fmt.Errorf("original StatefulSet %v/%v is gone: got uid %v, wanted %v", set.Namespace, set.Name, fresh.UID, set.UID)
		}
		return fresh, nil
	})
}

// adoptOrphanRevisions adopts any orphaned ControllerRevisions matched by set's Selector.
func (ssc *StatefulSetController) adoptOrphanRevisions(ctx context.Context, set *apps.StatefulSet) error {
	revisions, err := ssc.control.ListRevisions(set)
	if err != nil {
		return err
	}
	orphanRevisions := make([]*apps.ControllerRevision, 0)
	for i := range revisions {
		if metav1.GetControllerOf(revisions[i]) == nil {
			orphanRevisions = append(orphanRevisions, revisions[i])
		}
	}
	if len(orphanRevisions) > 0 {
		canAdoptErr := ssc.canAdoptFunc(ctx, set)(ctx)
		if canAdoptErr != nil {
			return fmt.Errorf("can't adopt ControllerRevisions: %v", canAdoptErr)
		}
		return ssc.control.AdoptOrphanRevisions(set, orphanRevisions)
	}
	return nil
}

// getStatefulSetsForPod returns a list of StatefulSets that potentially match
// a given pod.
func (ssc *StatefulSetController) getStatefulSetsForPod(pod *v1.Pod) []*apps.StatefulSet {
	sets, err := ssc.setLister.GetPodStatefulSets(pod)
	if err != nil {
		return nil
	}
	// More than one set is selecting the same Pod
	if len(sets) > 1 {
		// ControllerRef will ensure we don't do anything crazy, but more than one
		// item in this list nevertheless constitutes user error.
		setNames := []string{}
		for _, s := range sets {
			setNames = append(setNames, s.Name)
		}
		utilruntime.HandleError(
			fmt.Errorf(
				"user error: more than one StatefulSet is selecting pods with labels: %+v. Sets: %v",
				pod.Labels, setNames))
	}
	return sets
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (ssc *StatefulSetController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *apps.StatefulSet {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	set, err := ssc.setLister.StatefulSets(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if set.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return set
}

// enqueueStatefulSet enqueues the given statefulset in the work queue.
func (ssc *StatefulSetController) enqueueStatefulSet(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	ssc.queue.Add(key)
}

// enqueueStatefulSet enqueues the given statefulset in the work queue after given time
func (ssc *StatefulSetController) enqueueSSAfter(ss *apps.StatefulSet, duration time.Duration) {
	key, err := controller.KeyFunc(ss)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", ss, err))
		return
	}
	ssc.queue.AddAfter(key, duration)
}

// processNextWorkItem dequeues items, processes them, and marks them done. It enforces that the syncHandler is never
// invoked concurrently with the same key.
// processNextWorkItem 从工作队列中获取项目，处理它们，并标记为完成。
// 它确保相同的 key 不会并发执行 syncHandler。
func (ssc *StatefulSetController) processNextWorkItem(ctx context.Context) bool {
	// 1. 从队列中获取下一个待处理的 key
	// 如果队列已关闭，返回 false 表示应该停止工作
	key, quit := ssc.queue.Get()
	if quit {
		return false
	}
	// 2. 确保在处理完成后标记任务为完成
	// 使用 defer 确保即使发生 panic 也能正确标记
	defer ssc.queue.Done(key)
	// 3. 调用 sync 方法处理该 key 对应的 StatefulSet
	if err := ssc.sync(ctx, key); err != nil {
		// 3.1 如果处理失败，记录错误并重新入队
		utilruntime.HandleError(fmt.Errorf("error syncing StatefulSet %v, requeuing: %w", key, err))
		ssc.queue.AddRateLimited(key)
	} else {
		// 3.2 如果处理成功，标记任务为完成
		ssc.queue.Forget(key)
	}
	return true
}

// worker runs a worker goroutine that invokes processNextWorkItem until the controller's queue is closed
// worker 运行一个工作协程，持续调用 processNextWorkItem 直到控制器的队列关闭
func (ssc *StatefulSetController) worker(ctx context.Context) {
	// 持续调用 processNextWorkItem 处理队列中的任务
	// 当 processNextWorkItem 返回 false 时循环结束
	for ssc.processNextWorkItem(ctx) {
		// 循环体为空，所有工作都在 processNextWorkItem 中完成
	}
}

// sync syncs the given statefulset.
// sync 同步给定的 StatefulSet
func (ssc *StatefulSetController) sync(ctx context.Context, key string) error {
	startTime := time.Now()
	logger := klog.FromContext(ctx)
	defer func() {
		logger.V(4).Info("Finished syncing statefulset", "key", key, "time", time.Since(startTime))
	}()

	// 1. 解析 key 为 namespace 和 name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	// 2. 从缓存中获取 StatefulSet 对象
	set, err := ssc.setLister.StatefulSets(namespace).Get(name)
	if errors.IsNotFound(err) {
		logger.Info("StatefulSet has been deleted", "key", key)
		return nil
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to retrieve StatefulSet %v from store: %v", key, err))
		return err
	}

	// 3. 将 StatefulSet 的选择器转换为 LabelSelector
	selector, err := metav1.LabelSelectorAsSelector(set.Spec.Selector)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error converting StatefulSet %v selector: %v", key, err))
		// This is a non-transient error, so don't retry.
		return nil
	}

	// 4. 采用孤立的 ControllerRevision
	if err := ssc.adoptOrphanRevisions(ctx, set); err != nil {
		return err
	}

	// 5. 获取 StatefulSet 的 Pod 列表
	pods, err := ssc.getPodsForStatefulSet(ctx, set, selector)
	if err != nil {
		return err
	}

	// 6. 同步 StatefulSet 及其关联的 Pod 状态
	return ssc.syncStatefulSet(ctx, set, pods)
}

// syncStatefulSet syncs a tuple of (statefulset, []*v1.Pod).
// syncStatefulSet 同步 StatefulSet 及其关联的 Pod 状态
func (ssc *StatefulSetController) syncStatefulSet(ctx context.Context, set *apps.StatefulSet, pods []*v1.Pod) error {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Syncing StatefulSet with pods", "statefulSet", klog.KObj(set), "pods", len(pods))
	// 2. 声明变量用于存储状态和错误
	var status *apps.StatefulSetStatus
	var err error
	// 3. 调用控制接口更新 StatefulSet 状态
	status, err = ssc.control.UpdateStatefulSet(ctx, set, pods)
	if err != nil {
		return err
	}
	logger.V(4).Info("Successfully synced StatefulSet", "statefulSet", klog.KObj(set))
	// One more sync to handle the clock skew. This is also helping in requeuing right after status update
	// 5. 处理 MinReadySeconds 逻辑
	// 如果设置了 MinReadySeconds 并且可用副本数未达到期望值
	// 则延迟重新入队以处理时钟偏差
	if set.Spec.MinReadySeconds > 0 && status != nil && status.AvailableReplicas != *set.Spec.Replicas {
		ssc.enqueueSSAfter(set, time.Duration(set.Spec.MinReadySeconds)*time.Second)
	}

	return nil
}

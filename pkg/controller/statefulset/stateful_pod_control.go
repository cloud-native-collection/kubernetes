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
	"strings"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	errorutils "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
)

// StatefulPodControlObjectManager abstracts the manipulation of Pods and PVCs. The real controller implements this
// with a clientset for writes and listers for reads; for tests we provide stubs.
// StatefulPodControlObjectManager 抽象了对 Pod 和 PVC 的操作接口。
// 实际控制器使用 clientset 进行写操作，使用 listers 进行读操作来实现此接口；
// 在测试中，我们提供了存根实现。
type StatefulPodControlObjectManager interface {
	// 创建 Pod
	CreatePod(ctx context.Context, pod *v1.Pod) error
	// 获取 Pod
	GetPod(namespace, podName string) (*v1.Pod, error)
	// 更新 Pod
	UpdatePod(pod *v1.Pod) error
	// 删除 Pod
	DeletePod(pod *v1.Pod) error
	// 创建 PVC
	CreateClaim(claim *v1.PersistentVolumeClaim) error
	// 获取 PVC
	GetClaim(namespace, claimName string) (*v1.PersistentVolumeClaim, error)
	// 更新 PVC
	UpdateClaim(claim *v1.PersistentVolumeClaim) error
}

// StatefulPodControl defines the interface that StatefulSetController uses to create, update, and delete Pods,
// and to update the Status of a StatefulSet. It follows the design paradigms used for PodControl, but its
// implementation provides for PVC creation, ordered Pod creation, ordered Pod termination, and Pod identity enforcement.
// Manipulation of objects is provided through objectMgr, which allows the k8s API to be mocked out for testing.
// StatefulPodControl 定义了 StatefulSetController 用于创建、更新和删除 Pod 的接口，
// 以及更新 StatefulSet 状态的功能。它遵循了 PodControl 的设计模式，但其实现
// 提供了 PVC 创建、有序的 Pod 创建、有序的 Pod 终止以及 Pod 身份强制等功能。
// 通过 objectMgr 提供对象操作，这允许在测试中模拟 k8s API。
type StatefulPodControl struct {
	// 管理 Pod 和 PVC 的操作
	objectMgr StatefulPodControlObjectManager
	// 事件记录器
	recorder record.EventRecorder
}

// NewStatefulPodControl constructs a StatefulPodControl using a realStatefulPodControlObjectManager with the given
// clientset, listers and EventRecorder.
func NewStatefulPodControl(
	client clientset.Interface,
	podLister corelisters.PodLister,
	claimLister corelisters.PersistentVolumeClaimLister,
	recorder record.EventRecorder,
) *StatefulPodControl {
	return &StatefulPodControl{&realStatefulPodControlObjectManager{client, podLister, claimLister}, recorder}
}

// NewStatefulPodControlFromManager creates a StatefulPodControl using the given StatefulPodControlObjectManager and recorder.
func NewStatefulPodControlFromManager(om StatefulPodControlObjectManager, recorder record.EventRecorder) *StatefulPodControl {
	return &StatefulPodControl{om, recorder}
}

// realStatefulPodControlObjectManager uses a clientset.Interface and listers.
// 实现 StatefulPodControlObjectManager 接口，管理 Pod 和 PVC
type realStatefulPodControlObjectManager struct {
	client clientset.Interface
	// PodLister 用于获取 Pod
	podLister corelisters.PodLister
	// ClaimLister 用于获取 PVC
	claimLister corelisters.PersistentVolumeClaimLister
}

func (om *realStatefulPodControlObjectManager) CreatePod(ctx context.Context, pod *v1.Pod) error {
	_, err := om.client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

func (om *realStatefulPodControlObjectManager) GetPod(namespace, podName string) (*v1.Pod, error) {
	return om.podLister.Pods(namespace).Get(podName)
}

func (om *realStatefulPodControlObjectManager) UpdatePod(pod *v1.Pod) error {
	_, err := om.client.CoreV1().Pods(pod.Namespace).Update(context.TODO(), pod, metav1.UpdateOptions{})
	return err
}

func (om *realStatefulPodControlObjectManager) DeletePod(pod *v1.Pod) error {
	return om.client.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
}

func (om *realStatefulPodControlObjectManager) CreateClaim(claim *v1.PersistentVolumeClaim) error {
	_, err := om.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.TODO(), claim, metav1.CreateOptions{})
	return err
}

func (om *realStatefulPodControlObjectManager) GetClaim(namespace, claimName string) (*v1.PersistentVolumeClaim, error) {
	return om.claimLister.PersistentVolumeClaims(namespace).Get(claimName)
}

func (om *realStatefulPodControlObjectManager) UpdateClaim(claim *v1.PersistentVolumeClaim) error {
	_, err := om.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Update(context.TODO(), claim, metav1.UpdateOptions{})
	return err
}

// 创建 StatefulSet 的 Pod，并处理相关的 PVC 创建和策略设置
func (spc *StatefulPodControl) CreateStatefulPod(ctx context.Context, set *apps.StatefulSet, pod *v1.Pod) error {
	// Create the Pod's PVCs prior to creating the Pod
	// 创建 Pod 的 PVC，确保 PVC 存在
	if err := spc.createPersistentVolumeClaims(set, pod); err != nil {
		spc.recordPodEvent("create", set, pod, err)
		return err
	}
	// If we created the PVCs attempt to create the Pod
	// 通过 objectMgr 创建 Pod
	err := spc.objectMgr.CreatePod(ctx, pod)
	// sink already exists errors
	if apierrors.IsAlreadyExists(err) {
		return err
	}
	// 如果启用了 StatefulSetAutoDeletePVC，设置 PVC 的回收策略
	if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
		// Set PVC policy as much as is possible at this point.
		if err := spc.UpdatePodClaimForRetentionPolicy(ctx, set, pod); err != nil {
			spc.recordPodEvent("update", set, pod, err)
			return err
		}
	}
	spc.recordPodEvent("create", set, pod, err)
	return err
}

func (spc *StatefulPodControl) UpdateStatefulPod(ctx context.Context, set *apps.StatefulSet, pod *v1.Pod) error {
	attemptedUpdate := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// assume the Pod is consistent
		consistent := true
		// if the Pod does not conform to its identity, update the identity and dirty the Pod
		if !identityMatches(set, pod) {
			updateIdentity(set, pod)
			consistent = false
		}
		// if the Pod does not conform to the StatefulSet's storage requirements, update the Pod's PVC's,
		// dirty the Pod, and create any missing PVCs
		if !storageMatches(set, pod) {
			updateStorage(set, pod)
			consistent = false
			if err := spc.createPersistentVolumeClaims(set, pod); err != nil {
				spc.recordPodEvent("update", set, pod, err)
				return err
			}
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
			// if the Pod's PVCs are not consistent with the StatefulSet's PVC deletion policy, update the PVC
			// and dirty the pod.
			if match, err := spc.ClaimsMatchRetentionPolicy(ctx, set, pod); err != nil {
				spc.recordPodEvent("update", set, pod, err)
				return err
			} else if !match {
				if err := spc.UpdatePodClaimForRetentionPolicy(ctx, set, pod); err != nil {
					spc.recordPodEvent("update", set, pod, err)
					return err
				}
				consistent = false
			}
		}

		// if the Pod is not dirty, do nothing
		if consistent {
			return nil
		}

		attemptedUpdate = true
		// commit the update, retrying on conflicts

		updateErr := spc.objectMgr.UpdatePod(pod)
		if updateErr == nil {
			return nil
		}

		if updated, err := spc.objectMgr.GetPod(set.Namespace, pod.Name); err == nil {
			// make a copy so we don't mutate the shared cache
			pod = updated.DeepCopy()
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated Pod %s/%s: %w", set.Namespace, pod.Name, err))
		}

		return updateErr
	})
	if attemptedUpdate {
		spc.recordPodEvent("update", set, pod, err)
	}
	return err
}

// DeleteStatefulPod 删除 StatefulSet 中的 Pod
func (spc *StatefulPodControl) DeleteStatefulPod(set *apps.StatefulSet, pod *v1.Pod) error {
	err := spc.objectMgr.DeletePod(pod)
	spc.recordPodEvent("delete", set, pod, err)
	return err
}

// ClaimsMatchRetentionPolicy returns false if the PVCs for pod are not consistent with set's PVC deletion policy.
// An error is returned if something is not consistent. This is expected if the pod is being otherwise updated,
// but a problem otherwise (see usage of this method in UpdateStatefulPod).
func (spc *StatefulPodControl) ClaimsMatchRetentionPolicy(ctx context.Context, set *apps.StatefulSet, pod *v1.Pod) (bool, error) {
	logger := klog.FromContext(ctx)
	ordinal := getOrdinal(pod)
	templates := set.Spec.VolumeClaimTemplates
	for i := range templates {
		claimName := getPersistentVolumeClaimName(set, &templates[i], ordinal)
		claim, err := spc.objectMgr.GetClaim(set.Namespace, claimName)
		switch {
		case apierrors.IsNotFound(err):
			klog.FromContext(ctx).V(4).Info("Expected claim missing, continuing to pick up in next iteration", "PVC", klog.KObj(claim))
		case err != nil:
			return false, fmt.Errorf("Could not retrieve claim %s for %s when checking PVC deletion policy", claimName, pod.Name)
		default:
			if !isClaimOwnerUpToDate(logger, claim, set, pod) {
				return false, nil
			}
		}
	}
	return true, nil
}

// UpdatePodClaimForRetentionPolicy updates the PVCs used by pod to match the PVC deletion policy of set.
func (spc *StatefulPodControl) UpdatePodClaimForRetentionPolicy(ctx context.Context, set *apps.StatefulSet, pod *v1.Pod) error {
	logger := klog.FromContext(ctx)
	ordinal := getOrdinal(pod)
	templates := set.Spec.VolumeClaimTemplates
	for i := range templates {
		claimName := getPersistentVolumeClaimName(set, &templates[i], ordinal)
		claim, err := spc.objectMgr.GetClaim(set.Namespace, claimName)
		switch {
		case apierrors.IsNotFound(err):
			logger.V(4).Info("Expected claim missing, continuing to pick up in next iteration", "PVC", klog.KObj(claim))
		case err != nil:
			return fmt.Errorf("Could not retrieve claim %s not found for %s when checking PVC deletion policy: %w", claimName, pod.Name, err)
		default:
			if hasUnexpectedController(claim, set, pod) {
				// Add an event so the user knows they're in a strange configuration. The claim will be cleaned up below.
				msg := fmt.Sprintf("PersistentVolumeClaim %s has a conflicting OwnerReference that acts as a manging controller, the retention policy is ignored for this claim", claimName)
				spc.recorder.Event(set, v1.EventTypeWarning, "ConflictingController", msg)
			}
			if !isClaimOwnerUpToDate(logger, claim, set, pod) {
				claim = claim.DeepCopy() // Make a copy so we don't mutate the shared cache.
				updateClaimOwnerRefForSetAndPod(logger, claim, set, pod)
				if err := spc.objectMgr.UpdateClaim(claim); err != nil {
					return fmt.Errorf("could not update claim %s for delete policy ownerRefs: %w", claimName, err)
				}
			}
		}
	}
	return nil
}

// PodClaimIsStale returns true for a stale PVC that should block pod creation. If the scaling
// policy is deletion, and a PVC has an ownerRef that does not match the pod, the PVC is stale. This
// includes pods whose UID has not been created.
func (spc *StatefulPodControl) PodClaimIsStale(set *apps.StatefulSet, pod *v1.Pod) (bool, error) {
	policy := getPersistentVolumeClaimRetentionPolicy(set)
	if policy.WhenScaled == apps.RetainPersistentVolumeClaimRetentionPolicyType {
		// PVCs are meant to be reused and so can't be stale.
		return false, nil
	}
	for _, claim := range getPersistentVolumeClaims(set, pod) {
		pvc, err := spc.objectMgr.GetClaim(claim.Namespace, claim.Name)
		switch {
		case apierrors.IsNotFound(err):
			// If the claim doesn't exist yet, it can't be stale.
			continue
		case err != nil:
			return false, err
		case err == nil:
			if hasStaleOwnerRef(pvc, pod, podKind) {
				return true, nil
			}
		}
	}
	return false, nil
}

// recordPodEvent records an event for verb applied to a Pod in a StatefulSet. If err is nil the generated event will
// have a reason of v1.EventTypeNormal. If err is not nil the generated event will have a reason of v1.EventTypeWarning.
func (spc *StatefulPodControl) recordPodEvent(verb string, set *apps.StatefulSet, pod *v1.Pod, err error) {
	if err == nil {
		reason := fmt.Sprintf("Successful%s", strings.Title(verb))
		message := fmt.Sprintf("%s Pod %s in StatefulSet %s successful",
			strings.ToLower(verb), pod.Name, set.Name)
		spc.recorder.Event(set, v1.EventTypeNormal, reason, message)
	} else {
		reason := fmt.Sprintf("Failed%s", strings.Title(verb))
		message := fmt.Sprintf("%s Pod %s in StatefulSet %s failed error: %s",
			strings.ToLower(verb), pod.Name, set.Name, err)
		spc.recorder.Event(set, v1.EventTypeWarning, reason, message)
	}
}

// recordClaimEvent records an event for verb applied to the PersistentVolumeClaim of a Pod in a StatefulSet. If err is
// nil the generated event will have a reason of v1.EventTypeNormal. If err is not nil the generated event will have a
// reason of v1.EventTypeWarning.
func (spc *StatefulPodControl) recordClaimEvent(verb string, set *apps.StatefulSet, pod *v1.Pod, claim *v1.PersistentVolumeClaim, err error) {
	if err == nil {
		reason := fmt.Sprintf("Successful%s", strings.Title(verb))
		message := fmt.Sprintf("%s Claim %s Pod %s in StatefulSet %s success",
			strings.ToLower(verb), claim.Name, pod.Name, set.Name)
		spc.recorder.Event(set, v1.EventTypeNormal, reason, message)
	} else {
		reason := fmt.Sprintf("Failed%s", strings.Title(verb))
		message := fmt.Sprintf("%s Claim %s for Pod %s in StatefulSet %s failed error: %s",
			strings.ToLower(verb), claim.Name, pod.Name, set.Name, err)
		spc.recorder.Event(set, v1.EventTypeWarning, reason, message)
	}
}

// createMissingPersistentVolumeClaims creates all of the required PersistentVolumeClaims for pod, and updates its retention policy
func (spc *StatefulPodControl) createMissingPersistentVolumeClaims(ctx context.Context, set *apps.StatefulSet, pod *v1.Pod) error {
	if err := spc.createPersistentVolumeClaims(set, pod); err != nil {
		return err
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.StatefulSetAutoDeletePVC) {
		// Set PVC policy as much as is possible at this point.
		if err := spc.UpdatePodClaimForRetentionPolicy(ctx, set, pod); err != nil {
			spc.recordPodEvent("update", set, pod, err)
			return err
		}
	}
	return nil
}

// createPersistentVolumeClaims creates all of the required PersistentVolumeClaims for pod, which must be a member of
// set. If all of the claims for Pod are successfully created, the returned error is nil. If creation fails, this method
// may be called again until no error is returned, indicating the PersistentVolumeClaims for pod are consistent with
// set's Spec.
// createPersistentVolumeClaims 为 StatefulSet 的 Pod 创建所有必需的 PVC。
// 如果 Pod 的所有 PVC 都成功创建，则返回 nil 错误。如果创建失败，可以重试此方法，
// 直到返回 nil 错误，表示 Pod 的 PVC 已经与 StatefulSet 的 Spec 一致。
func (spc *StatefulPodControl) createPersistentVolumeClaims(set *apps.StatefulSet, pod *v1.Pod) error {
	var errs []error
	// 遍历 Pod 的所有 PVC
	for _, claim := range getPersistentVolumeClaims(set, pod) {
		// 获取 PVC
		pvc, err := spc.objectMgr.GetClaim(claim.Namespace, claim.Name)
		switch {
		// 情况1: PVC 不存在
		case apierrors.IsNotFound(err):
			err := spc.objectMgr.CreateClaim(&claim)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to create PVC %s: %s", claim.Name, err))
			}
			if err == nil || !apierrors.IsAlreadyExists(err) {
				spc.recordClaimEvent("create", set, pod, &claim, err)
			}
		// 情况2: PVC 已存在
		case err != nil:
			errs = append(errs, fmt.Errorf("failed to retrieve PVC %s: %s", claim.Name, err))
			spc.recordClaimEvent("create", set, pod, &claim, err)
		// 情况3: PVC 已删除
		default:
			if pvc.DeletionTimestamp != nil {
				errs = append(errs, fmt.Errorf("pvc %s is being deleted", claim.Name))
			}
		}
		// TODO: Check resource requirements and accessmodes, update if necessary
	}
	return errorutils.NewAggregate(errs)
}

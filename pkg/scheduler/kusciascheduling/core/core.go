/*
Copyright 2020 The Kubernetes Authors.

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

package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformer "k8s.io/client-go/informers/core/v1"
	kubelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	"github.com/secretflow/kuscia/pkg/common"
	kusciaapisv1alpha1 "github.com/secretflow/kuscia/pkg/crd/apis/kuscia/v1alpha1"
	kusciaclientset "github.com/secretflow/kuscia/pkg/crd/clientset/versioned"
	kusciainformer "github.com/secretflow/kuscia/pkg/crd/informers/externalversions/kuscia/v1alpha1"
	kuscialistersv1alpha1 "github.com/secretflow/kuscia/pkg/crd/listers/kuscia/v1alpha1"
	"github.com/secretflow/kuscia/pkg/utils/nlog"
	utilsres "github.com/secretflow/kuscia/pkg/utils/resources"
)

const (
	// defaultWaitTime is 60s if ResourceReservedSeconds is not specified.
	defaultWaitTime = 30 * time.Second

	retryInterval      = 1 * time.Second
	checkRetryInterval = 500 * time.Millisecond

	patchTimeout = 15 * time.Second
)

type Status string

const (
	// TaskResourceNotSpecified denotes no TaskResource is specified in the Pod spec.
	TaskResourceNotSpecified Status = "TaskResource not specified"
	// TaskResourceNotFound denotes the specified TaskResource in the Pod spec is not found in API server.
	TaskResourceNotFound Status = "TaskResource not found"
	Success              Status = "Success"
	Wait                 Status = "Wait"
)

// Manager defines the interfaces for TaskResource management.
type Manager interface {
	PreFilter(context.Context, *corev1.Pod) error
	Reserve(context.Context, *corev1.Pod)
	Unreserve(context.Context, *kusciaapisv1alpha1.TaskResource, *corev1.Pod)
	Permit(context.Context, *corev1.Pod) Status
	PreBind(context.Context, *corev1.Pod) (framework.Code, error)
	DeletePermittedTaskResource(*kusciaapisv1alpha1.TaskResource)
	CalculateAssignedPods(*kusciaapisv1alpha1.TaskResource, *corev1.Pod) int
	ActivateSiblings(*corev1.Pod, *framework.CycleState)
	GetTaskResource(*corev1.Pod) (string, *kusciaapisv1alpha1.TaskResource, bool)
}

// TaskResourceManager defines the scheduling operation called.
type TaskResourceManager struct {
	// kusciaClient is a kuscia client.
	kusciaClient kusciaclientset.Interface
	// snapshotSharedLister is pod shared list.
	snapshotSharedLister framework.SharedLister
	// trLister is TaskResource lister.
	trLister kuscialistersv1alpha1.TaskResourceLister
	// podLister is pod lister.
	podLister kubelisterv1.PodLister
	// nsLister is namespace lister
	nsLister kubelisterv1.NamespaceLister
	// key is <TaskResource namespace:name> and value is []taskResourceInfo.
	taskResourceInfos sync.Map
	// key is <TaskResource namespace:name> and value is patchTaskResourceInfo.
	patchTaskResourceInfos  sync.Map
	resourceReservedSeconds *time.Duration
}

// taskResourceInfo records task resource info.
type taskResourceInfo struct {
	nodeName string
	podName  string
}

type patchTaskResourceInfo struct {
	mu      sync.Mutex
	podName string
	err     error
}

// NewTaskResourceManager creates a new operation object.
func NewTaskResourceManager(kusciaClient kusciaclientset.Interface,
	snapshotSharedLister framework.SharedLister,
	trInformer kusciainformer.TaskResourceInformer,
	podInformer kubeinformer.PodInformer,
	nsInformer kubeinformer.NamespaceInformer,
	timeout *time.Duration) *TaskResourceManager {
	trMgr := &TaskResourceManager{
		kusciaClient:            kusciaClient,
		snapshotSharedLister:    snapshotSharedLister,
		trLister:                trInformer.Lister(),
		podLister:               podInformer.Lister(),
		nsLister:                nsInformer.Lister(),
		resourceReservedSeconds: timeout,
	}

	return trMgr
}

// PreFilter filters out a pod if
// 1. it belongs to a TaskResource that was recently denied or
// 2. the total number of pods in the TaskResource is less than the minimum number of pods
// that is required to be scheduled.
func (trMgr *TaskResourceManager) PreFilter(ctx context.Context, pod *corev1.Pod) error {
	ns, err := trMgr.nsLister.Get(pod.Namespace)
	if err != nil {
		return fmt.Errorf("failed to get pod %v/%v namespace", pod.Namespace, pod.Name)
	}

	if ns.Labels != nil && strings.ToLower(ns.Labels[common.LabelDomainRole]) == strings.ToLower(string(kusciaapisv1alpha1.Partner)) {
		return fmt.Errorf("skip schedule partner namespace %v pod", pod.Namespace)
	}

	trName, tr, labelExist := trMgr.GetTaskResource(pod)
	if !labelExist {
		return nil
	}

	if tr == nil {
		return fmt.Errorf("failed to get task resource %v/%v for pod", pod.Namespace, trName)
	}

	if tr.Status.Phase == "" ||
		tr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhasePending ||
		tr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseFailed {
		return fmt.Errorf("task resource %v status phase is %v, skip scheduling the pod", trName, tr.Status.Phase)
	}

	trLabel, _ := GetTaskResourceLabel(pod)
	pods, err := trMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{kusciaapisv1alpha1.LabelTaskResource: trLabel}),
	)

	if err != nil {
		return fmt.Errorf("podLister list pods failed, %v", err)
	}

	if len(pods) < tr.Spec.MinReservedPods {
		return fmt.Errorf("pre-filter pod %v cannot find enough sibling pods, "+
			"current pods number: %v, minReservedPods: %v", pod.Name, len(pods), tr.Spec.MinReservedPods)
	}

	return nil
}

// Reserve records task resource info of pod.
func (trMgr *TaskResourceManager) Reserve(ctx context.Context, pod *corev1.Pod) {
	trName, tr, _ := trMgr.GetTaskResource(pod)
	if tr == nil {
		return
	}

	nodeName := pod.Spec.NodeName
	podName := pod.Name
	if nodeName == "" {
		return
	}

	trInfo := taskResourceInfo{
		nodeName: nodeName,
		podName:  podName,
	}

	value, exist := trMgr.taskResourceInfos.Load(getTaskResourceInfoName(tr))
	if exist {
		trInfos, ok := value.([]taskResourceInfo)
		if !ok {
			nlog.Errorf("Reserve %s taskResourceInfo failed", trName)
			return
		}

		for i := range trInfos {
			if podName == trInfos[i].podName {
				trInfos = append(trInfos[:i], trInfos[i+1:]...)
				break
			}
		}

		trInfos = append(trInfos, trInfo)
		trMgr.taskResourceInfos.Store(getTaskResourceInfoName(tr), trInfos)
		return
	}

	trMgr.taskResourceInfos.Store(getTaskResourceInfoName(tr), []taskResourceInfo{trInfo})
}

// Unreserve is used to patch task resource status related to pod.
func (trMgr *TaskResourceManager) Unreserve(ctx context.Context, tr *kusciaapisv1alpha1.TaskResource, pod *corev1.Pod) {
	defer func() {
		trMgr.DeletePermittedTaskResource(tr)
	}()

	patchInfo, err := trMgr.getPatchTaskResourceInfos(getTaskResourceInfoName(tr))
	if err != nil {
		nlog.Errorf("get patch task resource infos failed, %v", err)
		return
	}

	patchInfo.mu.Lock()
	defer patchInfo.mu.Unlock()
	if patchInfo.podName == "" {
		patchInfo.podName = pod.Name
	} else if patchInfo.podName != pod.Name {
		return
	}

	patchFn := func() (bool, error) {
		if tr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseFailed {
			return true, nil
		}

		reason := fmt.Sprintf("schedule task resource %v/%v related pod %v/%v failed", tr.Namespace, tr.Name, pod.Namespace, pod.Name)
		if err := trMgr.patchTaskResource(kusciaapisv1alpha1.TaskResourcePhaseFailed, kusciaapisv1alpha1.TaskResourceCondFailed, reason, tr); err != nil {
			if k8serrors.IsNotFound(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	}

	go func() {
		nlog.Debugf("Patch task resource %v/%v to status phase to failed", tr.Namespace, tr.Name)
		err = wait.PollImmediate(retryInterval, patchTimeout, patchFn)
		nlog.Errorf("Patch task resource failed, %v", err)
	}()
}

// Permit permits a pod to run, if the minReservedPods match, it would send a signal to chan.
func (trMgr *TaskResourceManager) Permit(ctx context.Context, pod *corev1.Pod) Status {
	_, tr, labelExist := trMgr.GetTaskResource(pod)
	if !labelExist {
		return TaskResourceNotSpecified
	}

	if tr == nil {
		return TaskResourceNotFound
	}

	assigned := trMgr.CalculateAssignedPods(tr, pod)
	// The number of pods that have been assigned nodes is calculated from the snapshot.
	// The current pod in not included in the snapshot during the current scheduling cycle.
	if assigned+1 >= tr.Spec.MinReservedPods {
		return Success
	}
	return Wait
}

// PreBind is used to pre-check.
func (trMgr *TaskResourceManager) PreBind(ctx context.Context, pod *corev1.Pod) (framework.Code, error) {
	trName, tr, labelExist := trMgr.GetTaskResource(pod)
	if !labelExist {
		return framework.Success, nil
	}

	if tr == nil {
		return framework.Unschedulable, fmt.Errorf("does not find task resource %v/%v for pod %v/%v", pod.Namespace, trName, pod.Namespace, pod.Name)
	}

	start := time.Now()
	err := trMgr.patchTaskResourceWithPollImmediate(tr, pod)
	if err != nil {
		return framework.Unschedulable, err
	}

	reserveCostTime := time.Since(start).Seconds()
	totalTime := GetWaitTimeDuration(tr, trMgr.resourceReservedSeconds)
	schedulable, err := trMgr.isSchedulable(totalTime-time.Duration(reserveCostTime), tr, pod)
	if schedulable {
		return framework.Success, nil
	}

	nlog.Errorf("Can't schedule the pod %v/%v because of task resource status phase isn't schedulable, %v", pod.Namespace, pod.Name, err)
	return framework.Unschedulable, err
}

func (trMgr *TaskResourceManager) patchTaskResourceWithPollImmediate(tr *kusciaapisv1alpha1.TaskResource, pod *corev1.Pod) error {
	patchInfo, err := trMgr.getPatchTaskResourceInfos(getTaskResourceInfoName(tr))
	if err != nil {
		return err
	}

	patchInfo.mu.Lock()
	defer patchInfo.mu.Unlock()
	if patchInfo.podName == "" {
		patchInfo.podName = pod.Name
	} else if patchInfo.podName != pod.Name {
		return patchInfo.err
	}

	patchFn := func() (bool, error) {
		if tr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseReserved || tr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseSchedulable {
			return true, nil
		}

		if tr.Status.Phase != kusciaapisv1alpha1.TaskResourcePhaseReserving {
			return true, fmt.Errorf("task resource %v/%v status phase should be %v but get %v, skip scheduling the pod %v/%v",
				tr.Namespace, tr.Name, kusciaapisv1alpha1.TaskResourcePhaseReserving, tr.Status.Phase, pod.Namespace, pod.Name)
		}

		reason := "min member had reserved resource"
		if err := trMgr.patchTaskResource(kusciaapisv1alpha1.TaskResourcePhaseReserved, kusciaapisv1alpha1.TaskResourceCondReserved, reason, tr); err != nil {
			return false, nil
		}
		return true, nil
	}

	nlog.Infof("Patch task resource %v/%v status phase to reserved by pod %v/%v", tr.Namespace, tr.Name, pod.Namespace, pod.Name)
	patchInfo.err = wait.PollImmediate(retryInterval, GetWaitTimeDuration(tr, trMgr.resourceReservedSeconds), patchFn)
	trMgr.patchTaskResourceInfos.Store(getTaskResourceInfoName(tr), patchInfo)
	return err
}

func (trMgr *TaskResourceManager) patchTaskResource(phase kusciaapisv1alpha1.TaskResourcePhase, condType kusciaapisv1alpha1.TaskResourceConditionType, reason string, tr *kusciaapisv1alpha1.TaskResource) error {
	trCopy := tr.DeepCopy()
	trCopy.Status.Phase = phase

	if tr.Status.Phase != trCopy.Status.Phase {
		now := metav1.Now()
		cond := utilsres.GetTaskResourceCondition(&trCopy.Status, condType)
		cond.LastTransitionTime = &now
		cond.Status = corev1.ConditionTrue
		cond.Reason = reason
		trCopy.Status.LastTransitionTime = &now
		if err := utilsres.PatchTaskResource(context.Background(), trMgr.kusciaClient, utilsres.ExtractTaskResourceStatus(tr), utilsres.ExtractTaskResourceStatus(trCopy)); err != nil {
			return fmt.Errorf("patch task resource %v/%v status failed, %v", tr.Namespace, tr.Name, err.Error())
		}
	}
	return nil
}

func (trMgr *TaskResourceManager) isSchedulable(waitingTime time.Duration, tr *kusciaapisv1alpha1.TaskResource, pod *corev1.Pod) (bool, error) {
	var schedulable bool
	checkFn := func() (bool, error) {
		latestTr, err := trMgr.trLister.TaskResources(tr.Namespace).Get(tr.Name)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return false, err
			}
			return false, nil
		}

		if latestTr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseFailed {
			return false, fmt.Errorf("task resource status phase changed to failed")
		}

		if latestTr.Status.Phase == kusciaapisv1alpha1.TaskResourcePhaseSchedulable {
			schedulable = true
			return true, nil
		}

		nlog.Infof("Task resource %v/%v status is not %v for pod %v/%v, continue to wait...",
			latestTr.Namespace, latestTr.Name, kusciaapisv1alpha1.TaskResourcePhaseSchedulable, pod.Namespace, pod.Name)
		return false, nil
	}

	err := wait.PollImmediate(checkRetryInterval, waitingTime, checkFn)
	if wait.ErrWaitTimeout == err {
		siblingTrPhase := trMgr.getSiblingTaskResourcePhaseInfo(tr)
		if siblingTrPhase != nil {
			siblingTrPhaseInfo := ""
			for name, phase := range siblingTrPhase {
				siblingTrPhaseInfo += fmt.Sprintf("task resource %q phase is %q,", name, phase)
			}
			siblingTrPhaseInfo = strings.TrimSuffix(siblingTrPhaseInfo, ",")

			trgName := tr.Labels[common.LabelTaskResourceGroup]
			err = fmt.Errorf("reserved task resources belonging to the task resource group %q doesn't meet the minReservedMembers, %s", trgName, siblingTrPhaseInfo)
		}
	}

	if schedulable {
		return true, err
	}
	return false, err
}

// getSiblingTaskResourcePhaseInfo get sibling TaskResource phase info.
func (trMgr *TaskResourceManager) getSiblingTaskResourcePhaseInfo(tr *kusciaapisv1alpha1.TaskResource) map[string]string {
	if tr == nil || tr.Labels == nil {
		return nil
	}

	trgName := tr.Labels[common.LabelTaskResourceGroup]
	if trgName == "" {
		return nil
	}

	selector := labels.SelectorFromSet(labels.Set{common.LabelTaskResourceGroup: trgName})
	trs, err := trMgr.trLister.List(selector)
	if err != nil {
		return nil
	}

	trPhaseInfo := make(map[string]string)
	for _, item := range trs {
		if item.Name == tr.Name {
			continue
		}

		if item.Status.Phase != kusciaapisv1alpha1.TaskResourcePhaseReserved {
			key := item.Namespace + "/" + item.Name
			trPhaseInfo[key] = string(item.Status.Phase)
		}
	}
	return trPhaseInfo
}

// getPatchTaskResourceInfos gets patch task resource infos.
func (trMgr *TaskResourceManager) getPatchTaskResourceInfos(name string) (*patchTaskResourceInfo, error) {
	actual, _ := trMgr.patchTaskResourceInfos.LoadOrStore(name, &patchTaskResourceInfo{})
	patchInfo, ok := actual.(*patchTaskResourceInfo)
	if !ok {
		return nil, fmt.Errorf("value type %T is not patchTaskResourceInfo", actual)
	}

	return patchInfo, nil
}

// ActivateSiblings stashes the pods belonging to the same TaskResource of the given pod
// in the given state, with a reserved key "kubernetes.io/pods-to-activate".
func (trMgr *TaskResourceManager) ActivateSiblings(pod *corev1.Pod, state *framework.CycleState) {
	trName, _ := GetTaskResourceLabel(pod)
	if trName == "" {
		return
	}

	pods, err := trMgr.podLister.List(
		labels.SelectorFromSet(labels.Set{kusciaapisv1alpha1.LabelTaskResource: trName}),
	)

	if err != nil {
		nlog.Warnf("Failed to obtain pods belong to a taskResource %v/%v, %v", pod.Namespace, trName, err)
		return
	}

	for i := range pods {
		if pods[i].UID == pod.UID {
			pods = append(pods[:i], pods[i+1:]...)
			break
		}
	}

	if len(pods) != 0 {
		if c, err := state.Read(framework.PodsToActivateKey); err == nil {
			if s, ok := c.(*framework.PodsToActivate); ok {
				s.Lock()
				for _, p := range pods {
					namespacedName := fmt.Sprintf("%v/%v", p.GetNamespace(), p.GetName())
					s.Map[namespacedName] = p
				}
				s.Unlock()
			}
		}
	}
}

// DeletePermittedTaskResource deletes a TaskResource that passes Pre-Filter but reaches PostFilter.
func (trMgr *TaskResourceManager) DeletePermittedTaskResource(tr *kusciaapisv1alpha1.TaskResource) {
	trMgr.taskResourceInfos.Delete(getTaskResourceInfoName(tr))
	trMgr.patchTaskResourceInfos.Delete(getTaskResourceInfoName(tr))
}

// CalculateAssignedPods returns the number of pods that has been assigned nodes: assumed or bound.
func (trMgr *TaskResourceManager) CalculateAssignedPods(tr *kusciaapisv1alpha1.TaskResource, pod *corev1.Pod) int {
	snapshotReservedCount, trInfos := trMgr.getSnapshotReservedPodsCount(tr, pod)
	trLabel, _ := GetTaskResourceLabel(pod)
	pods, err := trMgr.podLister.Pods(pod.Namespace).List(
		labels.SelectorFromSet(labels.Set{kusciaapisv1alpha1.LabelTaskResource: trLabel}),
	)
	if err != nil {
		return snapshotReservedCount
	}

	schedPodsCount := getScheduledPodsCount(trInfos, pods)
	return snapshotReservedCount + schedPodsCount
}

// getSnapshotReservedPodsCount is used to get reserved pods count from snapshot.
func (trMgr *TaskResourceManager) getSnapshotReservedPodsCount(tr *kusciaapisv1alpha1.TaskResource, pod *corev1.Pod) (int, []taskResourceInfo) {
	value, found := trMgr.taskResourceInfos.Load(getTaskResourceInfoName(tr))
	if !found {
		nlog.Warnf("Can't find taskResourceInfos %v", getTaskResourceInfoName(tr))
		return 0, nil
	}

	trInfos, ok := value.([]taskResourceInfo)
	if !ok {
		nlog.Errorf("TaskResourceInfo %v type is invalid", getTaskResourceInfoName(tr))
		return 0, nil
	}

	var count int
	for _, trInfo := range trInfos {
		if trInfo.podName == pod.Name {
			continue
		}

		nodeInfo, err := trMgr.snapshotSharedLister.NodeInfos().Get(trInfo.nodeName)
		if err != nil {
			nlog.Warnf("Can't get node %s info from snapshotSharedLister, %v", trInfo.nodeName, err)
			continue
		}

		if nodeInfo == nil {
			nlog.Warnf("Get node %s info from snapshotSharedLister is empty", trInfo.nodeName)
			continue
		}

		for _, podInfo := range nodeInfo.Pods {
			if podInfo == nil || podInfo.Pod == nil {
				nlog.Warnf("Can't get pod task resource info on %s node", trInfo.nodeName)
				continue
			}

			if podInfo.Pod.Namespace == pod.Namespace && podInfo.Pod.Name == pod.Name {
				continue
			}

			if podInfo.Pod.Labels[kusciaapisv1alpha1.LabelTaskResource] == tr.Name && podInfo.Pod.Spec.NodeName != "" {
				count++
			}
		}
	}

	return count, trInfos
}

// getScheduledPodsCount return pods count which had scheduled.
func getScheduledPodsCount(trInfos []taskResourceInfo, pods []*corev1.Pod) int {
	var count int
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}

		found := false
		for _, trInfo := range trInfos {
			if trInfo.podName == pod.Name && trInfo.nodeName == pod.Spec.NodeName {
				found = true
				break
			}
		}

		if !found {
			count++
		}
	}

	return count
}

// GetTaskResource returns the task resource that a Pod belongs to in cache.
func (trMgr *TaskResourceManager) GetTaskResource(pod *corev1.Pod) (string, *kusciaapisv1alpha1.TaskResource, bool) {
	trName, exist := GetTaskResourceLabel(pod)
	if trName == "" {
		return trName, nil, exist
	}

	tr, err := trMgr.trLister.TaskResources(pod.Namespace).Get(trName)
	if err != nil {
		return trName, nil, exist
	}

	return trName, tr, exist
}

// GetTaskResourceLabel get task resource from pod annotations.
func GetTaskResourceLabel(pod *corev1.Pod) (string, bool) {
	value, exist := pod.Labels[kusciaapisv1alpha1.LabelTaskResource]
	return value, exist
}

// GetWaitTimeDuration returns a wait timeout based on the following precedences:
// 1. spec.resourceReservedSeconds of the given task resource, if specified
// 2. given resourceReservedSeconds, if not nil
// 3. fall back to defaultWaitTime
func GetWaitTimeDuration(tr *kusciaapisv1alpha1.TaskResource, resourceReservedSeconds *time.Duration) time.Duration {
	if tr != nil && tr.Spec.ResourceReservedSeconds > 0 {
		return time.Duration(tr.Spec.ResourceReservedSeconds) * time.Second
	}

	if resourceReservedSeconds != nil && *resourceReservedSeconds > 0 {
		return *resourceReservedSeconds * time.Second
	}

	return defaultWaitTime
}

// GetTaskResourceInfos returns task resource infos.
func GetTaskResourceInfos(trMgr *TaskResourceManager) *sync.Map {
	return &trMgr.taskResourceInfos
}

func getTaskResourceInfoName(tr *kusciaapisv1alpha1.TaskResource) string {
	return tr.Namespace + "/" + tr.Name
}

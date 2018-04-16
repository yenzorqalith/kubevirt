/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2017 Red Hat, Inc.
 *
 */

package watch

import (
	"time"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	virtv1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/log"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

// Reasons for vm events
const (
	// FailedCreatePodReason is added in an event and in a vm controller condition
	// when a pod for a vm controller failed to be created.
	FailedCreatePodReason = "FailedCreate"
	// SuccessfulCreatePodReason is added in an event when a pod for a vm controller
	// is successfully created.
	SuccessfulCreatePodReason = "SuccessfulCreate"
	// FailedDeletePodReason is added in an event and in a vm controller condition
	// when a pod for a vm controller failed to be deleted.
	FailedDeletePodReason = "FailedDelete"
	// SuccessfulDeletePodReason is added in an event when a pod for a vm controller
	// is successfully deleted.
	SuccessfulDeletePodReason = "SuccessfulDelete"
	// FailedHandOverPodReason is added in an event and in a vm controller condition
	// when transferring the pod ownership from the controller to virt-hander fails.
	FailedHandOverPodReason = "FailedHandOver"
	// SuccessfulHandOverPodReason is added in an event
	// when the pod ownership transfer from the controller to virt-hander succeeds.
	SuccessfulHandOverPodReason = "SuccessfulHandOver"
)

func NewVMController(templateService services.TemplateService, vmInformer cache.SharedIndexInformer, podInformer cache.SharedIndexInformer, recorder record.EventRecorder, clientset kubecli.KubevirtClient) *VMController {
	c := &VMController{
		vmService:            services.NewVMService(clientset, templateService),
		Queue:                workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		vmInformer:           vmInformer,
		podInformer:          podInformer,
		recorder:             recorder,
		clientset:            clientset,
		podExpectations:      controller.NewUIDTrackingControllerExpectations(controller.NewControllerExpectations()),
		handoverExpectations: controller.NewUIDTrackingControllerExpectations(controller.NewControllerExpectations()),
	}

	c.vmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addVirtualMachine,
		DeleteFunc: c.deleteVirtualMachine,
		UpdateFunc: c.updateVirtualMachine,
	})

	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPod,
		DeleteFunc: c.deletePod,
		UpdateFunc: c.updatePod,
	})

	return c
}

type VMController struct {
	vmService            services.VMService
	clientset            kubecli.KubevirtClient
	Queue                workqueue.RateLimitingInterface
	vmInformer           cache.SharedIndexInformer
	podInformer          cache.SharedIndexInformer
	recorder             record.EventRecorder
	podExpectations      *controller.UIDTrackingControllerExpectations
	handoverExpectations *controller.UIDTrackingControllerExpectations
}

func (c *VMController) Run(threadiness int, stopCh chan struct{}) {
	defer controller.HandlePanic()
	defer c.Queue.ShutDown()
	log.Log.Info("Starting controller.")

	// Wait for cache sync before we start the pod controller
	cache.WaitForCacheSync(stopCh, c.vmInformer.HasSynced, c.podInformer.HasSynced)

	// Start the actual work
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Log.Info("Stopping controller.")
}

func (c *VMController) runWorker() {
	for c.Execute() {
	}
}

func (c *VMController) Execute() bool {
	key, quit := c.Queue.Get()
	if quit {
		return false
	}
	defer c.Queue.Done(key)
	err := c.execute(key.(string))

	if err != nil {
		log.Log.Reason(err).Infof("reenqueuing VM %v", key)
		c.Queue.AddRateLimited(key)
	} else {
		log.Log.V(4).Infof("processed VM %v", key)
		c.Queue.Forget(key)
	}
	return true
}

func (c *VMController) execute(key string) error {

	// Fetch the latest Vm state from cache
	obj, exists, err := c.vmInformer.GetStore().GetByKey(key)

	if err != nil {
		return err
	}

	// Once all finalizers are removed the vm gets deleted and we can clean all expectations
	if !exists {
		c.podExpectations.DeleteExpectations(key)
		c.handoverExpectations.DeleteExpectations(key)
		return nil
	}
	vm := obj.(*virtv1.VirtualMachine)

	// If the VM is exists still, don't process the VM until it is fully initialized.
	// Initialization is handled by the initialization controller and must take place
	// before the VM is acted upon.
	if !isVirtualMachineInitialized(vm) {
		return nil
	}

	logger := log.Log.Object(vm)

	// Get all pods from the namespace
	pods, err := c.listPodsFromNamespace(vm.Namespace)

	if err != nil {
		logger.Reason(err).Error("Failed to fetch pods for namespace from cache.")
		return err
	}

	// Only consider pods which belong to this vm
	pods, err = c.filterMatchingPods(vm, pods)
	if err != nil {
		return err
	}

	if len(pods) > 1 {
		logger.Reason(fmt.Errorf("More than one pod detected")).Error("That should not be possible, will not requeue")
		return nil
	} else if len(pods) == 1 && pods[0].Annotations[virtv1.OwnedByAnnotation] == "virt-handler" {
		// Whenever we see that a matching pod is owned by virt-handler, we can mak handover as fulfilled
		c.handoverExpectations.CreationObserved(key)
	}

	// If neddsSync is true (expectations fulfilled) we can make save assumptions if virt-handler or virt-controller owns the pod
	needsSync := c.podExpectations.SatisfiedExpectations(key) && c.handoverExpectations.SatisfiedExpectations(key)

	var syncErr error
	if needsSync {
		syncErr = c.sync(vm, pods)
	}
	return c.updateStatus(vm, pods, syncErr)
}

func (c *VMController) updateStatus(vm *virtv1.VirtualMachine, pods []*k8sv1.Pod, syncErr error) error {

	// Don't process states where the vm is clearly owned by virt-handler
	if vm.IsRunning() || vm.IsScheduled() {
		return nil
	}

	vmCopy := vm.DeepCopy()

	if vm.IsUnprocessed() || vm.IsScheduling() {
		if len(pods) > 0 {

			containersAreReady := podIsReady(pods[0])

			podIsTerminating := podIsDownOrGoingDown(pods[0])

			// vm is still owned by the controller but pod is already handed over,
			// so let's hand over the vm too
			if c.isPodOwnedByHandler(pods[0]) {

				vmCopy.Status.Interfaces = []virtv1.VirtualMachineNetworkInterface{
					{
						IP: pods[0].Status.PodIP,
					},
				}
				vmCopy.Status.Phase = virtv1.Scheduled
				if vmCopy.Labels == nil {
					vmCopy.Labels = map[string]string{}
				}
				vmCopy.ObjectMeta.Labels[virtv1.NodeNameLabel] = pods[0].Spec.NodeName
				vmCopy.Status.NodeName = pods[0].Spec.NodeName
			} else if !podIsTerminating && !containersAreReady {
				vmCopy.Status.Phase = virtv1.Scheduling
			} else if podIsTerminating {
				vmCopy.Status.Phase = virtv1.Failed
			}
		} else if len(pods) == 0 && vm.IsScheduling() {
			// someone other than the controller deleted the pod unexpectedly
			vmCopy.Status.Phase = virtv1.Failed
		}
	} else if vm.IsFinal() {
		if len(pods) == 0 {
			controller.RemoveFinalizer(vmCopy, virtv1.VirtualMachineFinalizer)
		}
	}

	// Select the right failure reason in case we have an error
	reason := ""
	if len(pods) == 0 {
		reason = "FailedCreate"
	} else if vm.IsFinal() || vm.DeletionTimestamp != nil {
		reason = "FailedDelete"
	} else {
		reason = "FailedHandOver"
	}
	controller.NewVirtualMachineConditionManager().CheckFailure(vmCopy, syncErr, reason)

	// If we detect a change on the vm status, we update the vm
	if !reflect.DeepEqual(vm.Status, vmCopy.Status) || !reflect.DeepEqual(vm.Finalizers, vmCopy.Finalizers) {
		_, err := c.clientset.VM(vm.Namespace).Update(vmCopy)
		if err != nil {
			return err
		}
	}

	return nil
}

func podIsReady(pod *k8sv1.Pod) bool {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Ready == false {
			return false
		}
	}

	return pod.Status.Phase == k8sv1.PodRunning
}

func podIsDownOrGoingDown(pod *k8sv1.Pod) bool {
	return podIsDown(pod) || pod.DeletionTimestamp != nil
}

func podIsDown(pod *k8sv1.Pod) bool {
	return pod.Status.Phase == k8sv1.PodSucceeded || pod.Status.Phase == k8sv1.PodFailed
}

func (c *VMController) sync(vm *virtv1.VirtualMachine, pods []*k8sv1.Pod) (err error) {

	vmKey := controller.VirtualMachineKey(vm)

	if vm.IsFinal() || vm.DeletionTimestamp != nil {
		if len(pods) == 0 {
			return nil
		} else if pods[0].DeletionTimestamp == nil {
			c.podExpectations.ExpectDeletions(vmKey, []string{controller.PodKey(pods[0])})
			err := c.clientset.CoreV1().Pods(vm.Namespace).Delete(pods[0].Name, &v1.DeleteOptions{})
			if err != nil {
				c.recorder.Eventf(vm, k8sv1.EventTypeWarning, FailedDeletePodReason, "Error deleting pod: %v", err)
				c.podExpectations.DeletionObserved(vmKey, controller.PodKey(pods[0]))
				return err
			}
			c.recorder.Eventf(vm, k8sv1.EventTypeNormal, SuccessfulDeletePodReason, "Deleted virtual machine pod %s", pods[0].Name)
			return nil
		}
		return nil
	}

	if len(pods) == 0 {
		// If we came ever that far to detect that we already created a pod, we don't create it again
		if !vm.IsUnprocessed() {
			return nil
		}
		c.podExpectations.ExpectCreations(vmKey, 1)
		pod, err := c.vmService.StartVMPod(vm)
		if err != nil {
			c.recorder.Eventf(vm, k8sv1.EventTypeWarning, FailedCreatePodReason, "Error creating pod: %v", err)
			c.podExpectations.CreationObserved(vmKey)
			return err
		}
		c.recorder.Eventf(vm, k8sv1.EventTypeNormal, SuccessfulCreatePodReason, "Created virtual machine pod %s", pod.Name)
		return nil
	} else if len(pods) > 1 {
		return fmt.Errorf("Found %d matching pods where only one should exist", len(pods))
	} else if podIsReady(pods[0]) && !podIsDownOrGoingDown(pods[0]) && !c.isPodOwnedByHandler(pods[0]) {
		pod := pods[0].DeepCopy()
		pod.Annotations[virtv1.OwnedByAnnotation] = "virt-handler"
		c.handoverExpectations.ExpectCreations(controller.VirtualMachineKey(vm), 1)
		_, err := c.clientset.CoreV1().Pods(vm.Namespace).Update(pod)
		if err != nil {
			c.handoverExpectations.CreationObserved(controller.VirtualMachineKey(vm))
			c.recorder.Eventf(vm, k8sv1.EventTypeWarning, FailedHandOverPodReason, "Error on handing over pod: %v", err)
			return fmt.Errorf("failed to hand over pod to virt-handler: %v", err)
		}
		c.recorder.Eventf(vm, k8sv1.EventTypeNormal, SuccessfulHandOverPodReason, "Pod owner ship transfered to the node %s", pod.Name)
	}
	return nil
}

// When a pod is created, enqueue the replica set that manages it and update its podExpectations.
func (c *VMController) addPod(obj interface{}) {
	pod := obj.(*k8sv1.Pod)

	if pod.DeletionTimestamp != nil {
		// on a restart of the controller manager, it's possible a new pod shows up in a state that
		// is already pending deletion. Prevent the pod from being a creation observation.
		c.deletePod(pod)
		return
	}

	controllerRef := c.getControllerOf(pod)
	vm := c.resolveControllerRef(pod.Namespace, controllerRef)
	if vm == nil {
		return
	}
	vmKey, err := controller.KeyFunc(vm)
	if err != nil {
		return
	}
	log.Log.V(4).Object(pod).Infof("Pod created")
	c.podExpectations.CreationObserved(vmKey)
	c.enqueueVirtualMachine(vm)
}

// When a pod is updated, figure out what replica set/s manage it and wake them
// up. If the labels of the pod have changed we need to awaken both the old
// and new replica set. old and cur must be *v1.Pod types.
func (c *VMController) updatePod(old, cur interface{}) {
	curPod := cur.(*k8sv1.Pod)
	oldPod := old.(*k8sv1.Pod)
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have different RVs.
		return
	}

	labelChanged := !reflect.DeepEqual(curPod.Labels, oldPod.Labels)
	if curPod.DeletionTimestamp != nil {
		// when a pod is deleted gracefully it's deletion timestamp is first modified to reflect a grace period,
		// and after such time has passed, the virt-handler actually deletes it from the store. We receive an update
		// for modification of the deletion timestamp and expect an rs to create more replicas asap, not wait
		// until the virt-handler actually deletes the pod. This is different from the Phase of a pod changing, because
		// an rs never initiates a phase change, and so is never asleep waiting for the same.
		c.deletePod(curPod)
		if labelChanged {
			// we don't need to check the oldPod.DeletionTimestamp because DeletionTimestamp cannot be unset.
			c.deletePod(oldPod)
		}
		return
	}

	curControllerRef := c.getControllerOf(curPod)
	oldControllerRef := c.getControllerOf(oldPod)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		// The ControllerRef was changed. Sync the old controller, if any.
		if vm := c.resolveControllerRef(oldPod.Namespace, oldControllerRef); vm != nil {
			c.enqueueVirtualMachine(vm)
		}
	}

	vm := c.resolveControllerRef(curPod.Namespace, curControllerRef)
	if vm == nil {
		return
	}
	log.Log.V(4).Object(curPod).Infof("Pod updated")
	c.enqueueVirtualMachine(vm)
	// TODO: MinReadySeconds in the Pod will generate an Available condition to be added in
	// Update once we support the available conect on the vm
	return
}

// When a pod is deleted, enqueue the replica set that manages the pod and update its podExpectations.
// obj could be an *v1.Pod, or a DeletionFinalStateUnknown marker item.
func (c *VMController) deletePod(obj interface{}) {
	pod, ok := obj.(*k8sv1.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new vm will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			log.Log.Reason(fmt.Errorf("couldn't get object from tombstone %+v", obj)).Error("Failed to process delete notification")
			return
		}
		pod, ok = tombstone.Obj.(*k8sv1.Pod)
		if !ok {
			log.Log.Reason(fmt.Errorf("tombstone contained object that is not a pod %#v", obj)).Error("Failed to process delete notification")
			return
		}
	}

	controllerRef := c.getControllerOf(pod)
	vm := c.resolveControllerRef(pod.Namespace, controllerRef)
	if vm == nil {
		return
	}
	vmKey, err := controller.KeyFunc(vm)
	if err != nil {
		return
	}
	c.podExpectations.DeletionObserved(vmKey, controller.PodKey(pod))
	c.enqueueVirtualMachine(vm)
}

func (c *VMController) addVirtualMachine(obj interface{}) {
	c.enqueueVirtualMachine(obj)
}

func (c *VMController) deleteVirtualMachine(obj interface{}) {
	c.enqueueVirtualMachine(obj)
}

func (c *VMController) updateVirtualMachine(old, curr interface{}) {
	c.enqueueVirtualMachine(curr)
}

func (c *VMController) enqueueVirtualMachine(obj interface{}) {
	logger := log.Log
	vm := obj.(*virtv1.VirtualMachine)
	key, err := controller.KeyFunc(vm)
	if err != nil {
		logger.Object(vm).Reason(err).Error("Failed to extract key from virtualmachine.")
	}
	c.Queue.Add(key)
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (c *VMController) resolveControllerRef(namespace string, controllerRef *v1.OwnerReference) *virtv1.VirtualMachine {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != virtv1.VirtualMachineGroupVersionKind.Kind {
		return nil
	}
	vm, exists, err := c.vmInformer.GetStore().GetByKey(namespace + "/" + controllerRef.Name)
	if err != nil {
		return nil
	}
	if !exists {
		return nil
	}

	if vm.(*virtv1.VirtualMachine).UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return vm.(*virtv1.VirtualMachine)
}

// listPodsFromNamespace takes a namespace and returns all Pods from the pod cache which run in this namespace
func (c *VMController) listPodsFromNamespace(namespace string) ([]*k8sv1.Pod, error) {
	objs, err := c.podInformer.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	if err != nil {
		return nil, err
	}
	pods := []*k8sv1.Pod{}
	for _, obj := range objs {
		pod := obj.(*k8sv1.Pod)
		pods = append(pods, pod)
	}
	return pods, nil
}

func (c *VMController) filterMatchingPods(vm *virtv1.VirtualMachine, pods []*k8sv1.Pod) ([]*k8sv1.Pod, error) {
	selector, err := v1.LabelSelectorAsSelector(&v1.LabelSelector{MatchLabels: map[string]string{virtv1.DomainLabel: vm.Name, virtv1.AppLabel: "virt-launcher"}})
	if err != nil {
		return nil, err
	}
	matchingPods := []*k8sv1.Pod{}
	for _, pod := range pods {
		if selector.Matches(labels.Set(pod.ObjectMeta.Labels)) && pod.Annotations[virtv1.CreatedByAnnotation] == string(vm.UID) {
			matchingPods = append(matchingPods, pod)
		}
	}
	return matchingPods, nil
}

func (c *VMController) isPodOwnedByHandler(pod *k8sv1.Pod) bool {
	if pod.Annotations != nil && pod.Annotations[virtv1.OwnedByAnnotation] == "virt-handler" {
		return true
	}
	return false
}

func (c *VMController) getControllerOf(pod *k8sv1.Pod) *v1.OwnerReference {
	t := true
	return &v1.OwnerReference{
		Kind:               virtv1.VirtualMachineGroupVersionKind.Kind,
		Name:               pod.Labels[virtv1.DomainLabel],
		UID:                types.UID(pod.Annotations[virtv1.CreatedByAnnotation]),
		Controller:         &t,
		BlockOwnerDeletion: &t,
	}
}

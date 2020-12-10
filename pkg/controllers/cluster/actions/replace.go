// Copyright (C) 2017 ScyllaDB

package actions

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/scylladb/go-log"
	scyllav1alpha1 "github.com/scylladb/scylla-operator/pkg/api/v1alpha1"
	"github.com/scylladb/scylla-operator/pkg/controllers/cluster/util"
	"github.com/scylladb/scylla-operator/pkg/naming"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const RackReplaceNodeAction = "rack-replace-node"

type RackReplaceNode struct {
	Rack    scyllav1alpha1.RackSpec
	Cluster *scyllav1alpha1.ScyllaCluster
	Logger  log.Logger
}

var _ Action = &RackReplaceNode{}

// NewRackReplaceNodeAction returns action used for Scylla node replacement.
func NewRackReplaceNodeAction(r scyllav1alpha1.RackSpec, c *scyllav1alpha1.ScyllaCluster, l log.Logger) *RackReplaceNode {
	return &RackReplaceNode{
		Rack:    r,
		Cluster: c,
		Logger:  l,
	}
}

// Name returns name of the action.
func (a *RackReplaceNode) Name() string {
	return RackReplaceNodeAction
}

// Execute performs replace node operation.
// This action should be executed when at least one member service contain replace label.
// It will save IP address in Cluster status, delete PVC associated with Pod bound to marked member service
// which will release node affinity.
// Then Pod and member service itself will be deleted.
// Once StatefulSet controller creates new Pod and this Pod will enter ready state
// this action will cleanup replace label from member service, and replacement IP
// will be removed from Cluster status.
func (a *RackReplaceNode) Execute(ctx context.Context, s *State) error {
	a.Logger.Debug(ctx, "Replace action executed")

	r, c := a.Rack, a.Cluster

	// Find the member to decommission
	memberServices := &corev1.ServiceList{}

	err := s.List(ctx, memberServices, &client.ListOptions{
		LabelSelector: naming.RackSelector(r, c),
	})
	if err != nil {
		return errors.Wrap(err, "failed to list Member Service")
	}

	for _, member := range memberServices.Items {
		if value, ok := member.Labels[naming.ReplaceLabel]; ok {
			if value == "" {
				a.Logger.Debug(ctx, "Member needs to be replaced", "member", member.Name)
				if err := a.replaceNode(ctx, s, &member); err != nil {
					return errors.WithStack(err)
				}
			} else {
				a.Logger.Debug(ctx, "Member is being replaced", "member", member.Name)
				if err := a.maybeFinishReplaceNode(ctx, s, &member); err != nil {
					return errors.WithStack(err)
				}
			}
		}
	}

	return nil
}

func (a *RackReplaceNode) maybeFinishReplaceNode(ctx context.Context, state *State, member *corev1.Service) error {
	r, c, cc := a.Rack, a.Cluster, state.Client

	pod := &corev1.Pod{}
	err := cc.Get(ctx, naming.NamespacedName(member.Name, member.Namespace), pod)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrap(err, "get pod")
		}
		a.Logger.Info(ctx, "Member Pod not found", "member", member.Name)
	} else {
		if replaceAddr := member.Labels[naming.ReplaceLabel]; replaceAddr != "" {
			a.Logger.Info(ctx, "Replace member Pod found", "member", member.Name, "replace_address", replaceAddr, "ready", podReady(pod))
			if podReady(pod) {
				a.Logger.Info(ctx, "Replace member Pod ready, removing replace label", "member", member.Name, "replace_address", replaceAddr)

				old := member.DeepCopy()
				delete(member.Labels, naming.ReplaceLabel)
				if err := util.PatchService(ctx, old, member, state.kubeclient); err != nil {
					return errors.Wrap(err, "error patching member service")
				}

				a.Logger.Info(ctx, "Removing replace IP from Cluster status", "member", member.Name)
				delete(c.Status.Racks[r.Name].ReplaceAddressFirstBoot, member.Name)

				state.recorder.Event(c, corev1.EventTypeNormal, naming.SuccessSynced,
					fmt.Sprintf("Rack %q replaced %q node", r.Name, member.Name),
				)
			}
		}
	}

	return nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

const (
	retryInterval     = 200 * time.Millisecond
	waitForPVCTimeout = 30 * time.Second
)

func waitForPVC(ctx context.Context, cc client.Client, name, namespace string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	return wait.PollImmediate(retryInterval, waitForPVCTimeout, func() (bool, error) {
		err := cc.Get(ctx, naming.NamespacedName(name, namespace), pvc)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

func (a *RackReplaceNode) replaceNode(ctx context.Context, state *State, member *corev1.Service) error {
	r, c := a.Rack, a.Cluster

	cc := state.Client

	// Save replace address in RackStatus
	rackStatus := c.Status.Racks[r.Name]
	rackStatus.ReplaceAddressFirstBoot[member.Name] = member.Spec.ClusterIP
	a.Logger.Debug(ctx, "Adding member address to replace address list", "member", member.Name, "ip", member.Spec.ClusterIP, "replace_addresses", rackStatus.ReplaceAddressFirstBoot)

	// Proceed to destructive operations only when IP address is saved in cluster Status.
	if err := cc.Status().Update(ctx, c); err != nil {
		return errors.Wrap(err, "failed to delete pvc")
	}

	// Delete PVC if it exists
	pvc := &corev1.PersistentVolumeClaim{}
	err := cc.Get(ctx, naming.NamespacedName(naming.PVCNameForPod(member.Name), member.Namespace), pvc)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to get pvc")
		}
		a.Logger.Info(ctx, "Member PVC not found", "member", member.Name)
	} else {
		a.Logger.Info(ctx, "Deleting member PVC", "member", member.Name, "pvc", pvc.Name)
		if err = cc.Delete(ctx, pvc); err != nil {
			return errors.Wrap(err, "failed to delete pvc")
		}

		// Wait until PVC is deleted, ignore error
		a.Logger.Info(ctx, "Waiting for PVC deletion", "member", member.Name, "pvc", pvc.Name)
		_ = waitForPVC(ctx, cc, naming.PVCNameForPod(member.Name), member.Namespace)

		state.recorder.Event(c, corev1.EventTypeNormal, naming.SuccessSynced,
			fmt.Sprintf("Rack %q removed %q PVC", r.Name, member.Name),
		)
	}

	// Delete Pod if it exists
	pod := &corev1.Pod{}
	err = cc.Get(ctx, naming.NamespacedName(member.Name, member.Namespace), pod)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return errors.Wrap(err, "get pod")
		}
		a.Logger.Info(ctx, "Member Pod not found", "member", member.Name)
	} else {
		a.Logger.Info(ctx, "Deleting member Pod", "member", member.Name)
		if err = cc.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
			return errors.Wrap(err, "delete pod")
		}
		state.recorder.Event(c, corev1.EventTypeNormal, naming.SuccessSynced,
			fmt.Sprintf("Rack %q removed %q Pod", r.Name, member.Name),
		)
	}

	// Delete member Service
	a.Logger.Info(ctx, "Deleting member Service", "member", member.Name)
	if err := cc.Delete(ctx, member); err != nil {
		return errors.Wrap(err, "delete member service")
	}

	state.recorder.Event(c, corev1.EventTypeNormal, naming.SuccessSynced,
		fmt.Sprintf("Rack %q removed %q Service", r.Name, member.Name),
	)

	return nil
}

/*
Copyright 2026.

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

package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// placementRecheckInterval backstops the node watch: LINSTOR primaries can
// move without any k8s event.
const placementRecheckInterval = time.Minute

// unreadyNodes lists nodes that must not serve exports; placement avoids
// them, which is what moves a target or share off a dead node.
func unreadyNodes(ctx context.Context, c client.Client) (map[string]bool, error) {
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return nil, err
	}
	down := map[string]bool{}
	for _, n := range nodes.Items {
		if !nodeReady(&n) {
			down[n.Name] = true
		}
	}
	return down, nil
}

// nodeGoneOrUnready reports whether name is absent from the cluster or
// NotReady: either way its agent cannot confirm a teardown, so deletion
// finalizers must release without one. unreadyNodes cannot answer this:
// it only maps Nodes that still exist, and a decommissioned node would
// hold a finalizer forever.
func nodeGoneOrUnready(ctx context.Context, c client.Client, name string) (bool, error) {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !nodeReady(&n), nil
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeReadyChanged fires only on readiness flips and node removal; every
// other node update is noise for placement.
var nodeReadyChanged = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldNode, okOld := e.ObjectOld.(*corev1.Node)
		newNode, okNew := e.ObjectNew.(*corev1.Node)
		return okOld && okNew && nodeReady(oldNode) != nodeReady(newNode)
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

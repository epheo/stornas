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

package rdrbd

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rdrbdv1alpha1 "github.com/epheo/stornas/operator/api/rdrbd/v1alpha1"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

func storagePool() *storagev1alpha1.StoragePool {
	return &storagev1alpha1.StoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tank"},
		Spec:       storagev1alpha1.StoragePoolSpec{Node: "node-a", Tier: "ssd"},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := rdrbdv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&rdrbdv1alpha1.Pool{}).Build()
}

func pool(spec rdrbdv1alpha1.PoolSpec, available *metav1.Condition, volumes ...string) *rdrbdv1alpha1.Pool {
	p := &rdrbdv1alpha1.Pool{ObjectMeta: metav1.ObjectMeta{Name: "tank"}, Spec: spec}
	if available != nil {
		available.Type = rdrbdv1alpha1.ConditionAvailable
		available.Reason = "Test"
		meta.SetStatusCondition(&p.Status.Conditions, *available)
	}
	p.Status.Volumes = volumes
	return p
}

func TestEnsurePoolCreatesWhenAbsent(t *testing.T) {
	c := newClient(t)
	ready, msg, err := EnsurePool(context.Background(), c, storagePool())
	if err != nil {
		t.Fatal(err)
	}
	// A fresh create is not usable until the agent reports Available.
	if ready || msg == "" {
		t.Fatalf("ready=%v msg=%q", ready, msg)
	}
	var got rdrbdv1alpha1.Pool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tank"}, &got); err != nil {
		t.Fatal(err)
	}
	want := rdrbdv1alpha1.PoolSpec{Node: "node-a", Tier: "ssd", VG: "stornas-tank", ThinPool: "thin"}
	if got.Spec != want {
		t.Fatalf("spec = %+v", got.Spec)
	}
}

func TestEnsurePoolDefaultsTier(t *testing.T) {
	sp := storagePool()
	sp.Spec.Tier = ""
	if PoolSpec(sp).Tier != storagev1alpha1.DefaultTier {
		t.Fatalf("tier = %q", PoolSpec(sp).Tier)
	}
}

func TestEnsurePoolReadyFollowsAvailable(t *testing.T) {
	spec := PoolSpec(storagePool())
	cases := map[string]struct {
		available *metav1.Condition
		ready     bool
		msg       string
	}{
		"unreported": {nil, false, "waiting"},
		"false":      {&metav1.Condition{Status: metav1.ConditionFalse, Message: "thin pool not found"}, false, "thin pool not found"},
		"true":       {&metav1.Condition{Status: metav1.ConditionTrue}, true, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, pool(spec, tc.available))
			ready, msg, err := EnsurePool(context.Background(), c, storagePool())
			if err != nil {
				t.Fatal(err)
			}
			if ready != tc.ready || !strings.Contains(msg, tc.msg) {
				t.Fatalf("ready=%v msg=%q", ready, msg)
			}
		})
	}
}

// A Pool of the same name on another VG was made by hand; adopting it
// would hand its volumes to this pool.
func TestEnsurePoolRefusesForeignPool(t *testing.T) {
	spec := PoolSpec(storagePool())
	spec.VG = "stornas-other"
	c := newClient(t, pool(spec, &metav1.Condition{Status: metav1.ConditionTrue}))
	ready, msg, err := EnsurePool(context.Background(), c, storagePool())
	if err != nil {
		t.Fatal(err)
	}
	if ready || !strings.Contains(msg, "stornas-other") {
		t.Fatalf("ready=%v msg=%q", ready, msg)
	}
}

func TestDeletePoolGoneWhenAbsent(t *testing.T) {
	gone, _, err := DeletePool(context.Background(), newClient(t), storagePool())
	if err != nil || !gone {
		t.Fatalf("gone=%v err=%v", gone, err)
	}
}

// The guard: r-drbd holds the Pool while volumes use it, and the delete
// reports which ones.
func TestDeletePoolWaitsForVolumes(t *testing.T) {
	p := pool(PoolSpec(storagePool()), nil, "pvc-1")
	p.Finalizers = []string{"r-drbd.io/pool"}
	c := newClient(t, p)
	gone, msg, err := DeletePool(context.Background(), c, storagePool())
	if err != nil {
		t.Fatal(err)
	}
	if gone || !strings.Contains(msg, "pvc-1") {
		t.Fatalf("gone=%v msg=%q", gone, msg)
	}
	var got rdrbdv1alpha1.Pool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tank"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.DeletionTimestamp.IsZero() {
		t.Fatal("pool not marked for deletion")
	}
}

func TestDeletePoolLeavesForeignPool(t *testing.T) {
	spec := PoolSpec(storagePool())
	spec.VG = "stornas-other"
	c := newClient(t, pool(spec, nil))
	gone, _, err := DeletePool(context.Background(), c, storagePool())
	if err != nil || !gone {
		t.Fatalf("gone=%v err=%v", gone, err)
	}
	var got rdrbdv1alpha1.Pool
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tank"}, &got); err != nil {
		t.Fatal(err)
	}
	if !got.DeletionTimestamp.IsZero() {
		t.Fatal("foreign pool deleted")
	}
}

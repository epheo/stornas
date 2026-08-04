// Package clusterstate maintains stornas's single source of live cluster
// truth: reflectors run ONCE under the server's ServiceAccount and keep
// in-memory indexers of StoragePools, Nodes, and PVCs. A read is a pure
// in-memory scan of this snapshot - the cluster is never touched on the
// read path, so any number of UI viewers cost one watch set. On any
// mutation a reflector publishes its kind to the shared event bus; the WS
// hub subscribes and rebuilds. Same architecture as dotvirt's clusterstate,
// rebuilt for storage resources.
package clusterstate

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"

	"github.com/epheo/stornas/internal/eventbus"
	"github.com/epheo/stornas/internal/model"
	"github.com/epheo/stornas/internal/reflect"
)

var poolGVR = schema.GroupVersionResource{
	Group:    storagev1alpha1.GroupVersion.Group,
	Version:  storagev1alpha1.GroupVersion.Version,
	Resource: "storagepools",
}

var shareGVR = schema.GroupVersionResource{
	Group:    storagev1alpha1.GroupVersion.Group,
	Version:  storagev1alpha1.GroupVersion.Version,
	Resource: "shares",
}

var inventoryGVR = schema.GroupVersionResource{
	Group:    storagev1alpha1.GroupVersion.Group,
	Version:  storagev1alpha1.GroupVersion.Version,
	Resource: "nodeinventories",
}

// State is the SA-maintained snapshot. Build with New, start with Run;
// Snapshot reads are lock-free indexer scans safe for concurrent callers.
type State struct {
	pools       cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	shares      cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	inventories cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	nodes       cache.Indexer // *corev1.Node
	pvcs        cache.Indexer // *corev1.PersistentVolumeClaim

	specs []reflectorSpec

	poolsSynced, sharesSynced, invSynced, nodesSynced, pvcsSynced atomic.Bool
	syncedOnce                                                    sync.Once
	allSynced                                                     chan struct{}

	healthy atomic.Bool // any reflector's List/Watch failing flips this false
}

type reflectorSpec struct {
	store    cache.Store
	expected any
	lw       cache.ListerWatcher
}

// New builds the snapshot's reflectors: StoragePools via the dynamic client
// (the CRD types live in the operator module, but reflectors want plain
// list/watch), Nodes and PVCs via the typed clientset. bus is optional
// (nil disables signalling, e.g. in tests).
func New(cs kubernetes.Interface, dyn dynamic.Interface, bus *eventbus.Bus) *State {
	s := &State{allSynced: make(chan struct{})}
	s.pools = newIndexer()
	s.shares = newIndexer()
	s.inventories = newIndexer()
	s.nodes = newIndexer()
	s.pvcs = newIndexer()
	s.healthy.Store(true)

	pool := func() { bus.Publish(eventbus.PoolChanged) }
	shareFn := func() { bus.Publish(eventbus.ShareChanged) }
	node := func() { bus.Publish(eventbus.NodeChanged) }
	volume := func() { bus.Publish(eventbus.VolumeChanged) }

	s.specs = []reflectorSpec{
		{
			reflect.NewStore(s.pools, pool, func() { s.poolsSynced.Store(true); s.checkSynced() }),
			&unstructured.Unstructured{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return dyn.Resource(poolGVR).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return dyn.Resource(poolGVR).Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			reflect.NewStore(s.shares, shareFn, func() { s.sharesSynced.Store(true); s.checkSynced() }),
			&unstructured.Unstructured{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return dyn.Resource(shareGVR).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return dyn.Resource(shareGVR).Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			// Inventory moves feed the same NodeChanged kind: the UI's
			// node view is the join of both objects.
			reflect.NewStore(s.inventories, node, func() { s.invSynced.Store(true); s.checkSynced() }),
			&unstructured.Unstructured{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return dyn.Resource(inventoryGVR).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return dyn.Resource(inventoryGVR).Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			reflect.NewStore(s.nodes, node, func() { s.nodesSynced.Store(true); s.checkSynced() }),
			&corev1.Node{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return cs.CoreV1().Nodes().List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return cs.CoreV1().Nodes().Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			reflect.NewStore(s.pvcs, volume, func() { s.pvcsSynced.Store(true); s.checkSynced() }),
			&corev1.PersistentVolumeClaim{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return cs.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return cs.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).Watch(ctx, o)
				},
			}, &s.healthy),
		},
	}
	return s
}

func newIndexer() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
}

// Run starts one reflector per resource; each owns its own relist/backoff
// and stops when ctx is cancelled. Returns immediately - call WaitForSync
// to block until the initial LIST has populated the snapshot.
func (s *State) Run(ctx context.Context) {
	for _, spec := range s.specs {
		r := cache.NewReflector(spec.lw, spec.expected, spec.store, 0)
		go r.Run(ctx.Done())
	}
}

// WaitForSync blocks until every reflector's initial LIST has landed or ctx
// is done.
func (s *State) WaitForSync(ctx context.Context) error {
	select {
	case <-s.allSynced:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *State) checkSynced() {
	if s.poolsSynced.Load() && s.sharesSynced.Load() && s.invSynced.Load() && s.nodesSynced.Load() && s.pvcsSynced.Load() {
		s.syncedOnce.Do(func() { close(s.allSynced) })
	}
}

// Healthy reports whether the watches are established; false means the
// snapshot keeps serving last-good contents that may be stale.
func (s *State) Healthy() bool {
	return s.healthy.Load()
}

// Snapshot builds one consistent UI frame from the indexers. Pure
// in-memory; sorted so identical cluster state yields identical frames
// (the WS hub dedupes on the encoded bytes).
func (s *State) Snapshot() model.Snapshot {
	var snap model.Snapshot
	for _, u := range reflect.List(s.pools) {
		var pool storagev1alpha1.StoragePool
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &pool); err != nil {
			continue
		}
		snap.Pools = append(snap.Pools, poolModel(&pool))
	}
	disksByNode := map[string][]model.Disk{}
	for _, u := range reflect.List(s.inventories) {
		var inv storagev1alpha1.NodeInventory
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &inv); err != nil {
			continue
		}
		for _, d := range inv.Status.Disks {
			disk := model.Disk{
				Path:       d.Path,
				Model:      d.Model,
				Serial:     d.Serial,
				Rotational: d.Rotational,
				Claimed:    d.Claimed,
			}
			if d.Size != nil {
				disk.SizeBytes = d.Size.Value()
			}
			disksByNode[inv.Name] = append(disksByNode[inv.Name], disk)
		}
	}
	for _, obj := range s.nodes.List() {
		n, ok := obj.(*corev1.Node)
		if !ok {
			continue
		}
		nm := nodeModel(n)
		nm.Disks = disksByNode[n.Name]
		snap.Nodes = append(snap.Nodes, nm)
	}
	for _, obj := range s.pvcs.List() {
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			continue
		}
		snap.Volumes = append(snap.Volumes, volumeModel(pvc))
	}
	for _, u := range reflect.List(s.shares) {
		var share storagev1alpha1.Share
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &share); err != nil {
			continue
		}
		snap.Shares = append(snap.Shares, shareModel(&share))
	}
	sort.Slice(snap.Shares, func(i, j int) bool {
		a, b := snap.Shares[i], snap.Shares[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	sort.Slice(snap.Pools, func(i, j int) bool { return snap.Pools[i].Name < snap.Pools[j].Name })
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })
	sort.Slice(snap.Volumes, func(i, j int) bool {
		a, b := snap.Volumes[i], snap.Volumes[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	return snap
}

func poolModel(p *storagev1alpha1.StoragePool) model.Pool {
	out := model.Pool{
		Name:    p.Name,
		Node:    p.Spec.Node,
		Raid:    p.Spec.Raid,
		VG:      p.Status.VG,
		Health:  p.Status.Health,
		Linstor: p.Status.LinstorPool,
	}
	if out.Health == "" {
		out.Health = "Unknown"
	}
	if p.Status.Capacity != nil {
		out.CapacityBytes = p.Status.Capacity.Value()
	}
	if p.Status.Free != nil {
		out.FreeBytes = p.Status.Free.Value()
	}
	observed := map[string]storagev1alpha1.DeviceStatus{}
	for _, d := range p.Status.Devices {
		observed[d.Path] = d
	}
	// Spec order, status detail: the UI lists what the user declared even
	// before the agent has reported (state stays empty until then).
	for _, path := range p.Spec.Devices {
		dev := model.Device{Path: path}
		if d, ok := observed[path]; ok {
			dev.State = d.State
			dev.Smart = d.Smart
		}
		out.Devices = append(out.Devices, dev)
	}
	for _, c := range p.Status.Conditions {
		if c.Type == storagev1alpha1.ConditionAvailable {
			out.Available = c.Status == metav1.ConditionTrue
			out.Reason = c.Reason
		}
	}
	return out
}

func shareModel(s *storagev1alpha1.Share) model.Share {
	out := model.Share{
		Namespace: s.Namespace,
		Name:      s.Name,
		Claim:     s.Spec.ClaimName,
		NFS:       s.Spec.NFS != nil,
		SMB:       s.Spec.SMB != nil,
		Node:      s.Status.Node,
		State:     s.Status.State,
	}
	if out.State == "" {
		out.State = "Pending"
	}
	for _, c := range s.Status.Conditions {
		if c.Type == storagev1alpha1.ConditionAvailable {
			out.Available = c.Status == metav1.ConditionTrue
			out.Reason = c.Reason
		}
	}
	return out
}

func nodeModel(n *corev1.Node) model.Node {
	out := model.Node{
		Name:           n.Name,
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			out.Ready = c.Status == corev1.ConditionTrue
		}
	}
	for label := range n.Labels {
		if role, ok := roleFromLabel(label); ok {
			out.Roles = append(out.Roles, role)
		}
	}
	sort.Strings(out.Roles)
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
			out.Addresses = append(out.Addresses, a.Address)
		}
	}
	return out
}

const rolePrefix = "node-role.kubernetes.io/"

func roleFromLabel(label string) (string, bool) {
	if len(label) > len(rolePrefix) && label[:len(rolePrefix)] == rolePrefix {
		return label[len(rolePrefix):], true
	}
	return "", false
}

func volumeModel(pvc *corev1.PersistentVolumeClaim) model.Volume {
	out := model.Volume{
		Namespace: pvc.Namespace,
		Name:      pvc.Name,
		Phase:     string(pvc.Status.Phase),
		Block:     pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock,
		Resource:  pvc.Spec.VolumeName,
	}
	if pvc.Spec.StorageClassName != nil {
		out.StorageClass = *pvc.Spec.StorageClassName
	}
	if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		out.CapacityBytes = q.Value()
	} else if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		out.CapacityBytes = q.Value()
	}
	return out
}

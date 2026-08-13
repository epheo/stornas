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
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
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

var targetGVR = schema.GroupVersionResource{
	Group:    storagev1alpha1.GroupVersion.Group,
	Version:  storagev1alpha1.GroupVersion.Version,
	Resource: "targets",
}

var snapshotGVR = schema.GroupVersionResource{
	Group:    "snapshot.storage.k8s.io",
	Version:  "v1",
	Resource: "volumesnapshots",
}

// State is the SA-maintained snapshot. Build with New, start with Run;
// Snapshot reads are lock-free indexer scans safe for concurrent callers.
type State struct {
	pools       cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	shares      cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	inventories cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	targets     cache.Indexer // *unstructured.Unstructured (CRD via dynamic client)
	snapshots   cache.Indexer // *unstructured.Unstructured (CSI snapshots via dynamic client)
	nodes       cache.Indexer // *corev1.Node
	pvcs        cache.Indexer // *corev1.PersistentVolumeClaim
	events      cache.Indexer // *corev1.Event (Warning only)

	specs []reflectorSpec

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
	s := &State{}
	s.pools = newIndexer()
	s.shares = newIndexer()
	s.inventories = newIndexer()
	s.targets = newIndexer()
	s.snapshots = newIndexer()
	s.nodes = newIndexer()
	s.pvcs = newIndexer()
	s.events = newIndexer()
	s.healthy.Store(true)

	pool := func() { bus.Publish(eventbus.PoolChanged) }
	shareFn := func() { bus.Publish(eventbus.ShareChanged) }
	node := func() { bus.Publish(eventbus.NodeChanged) }
	volume := func() { bus.Publish(eventbus.VolumeChanged) }
	target := func() { bus.Publish(eventbus.TargetChanged) }
	snapFn := func() { bus.Publish(eventbus.SnapshotChanged) }
	alert := func() { bus.Publish(eventbus.AlertChanged) }

	s.specs = []reflectorSpec{
		{
			reflect.NewStore(s.pools, pool, nil),
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
			reflect.NewStore(s.shares, shareFn, nil),
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
			reflect.NewStore(s.inventories, node, nil),
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
			reflect.NewStore(s.targets, target, nil),
			&unstructured.Unstructured{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return dyn.Resource(targetGVR).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return dyn.Resource(targetGVR).Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			reflect.NewStore(s.snapshots, snapFn, nil),
			&unstructured.Unstructured{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					return dyn.Resource(snapshotGVR).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					return dyn.Resource(snapshotGVR).Watch(ctx, o)
				},
			}, &s.healthy),
		},
		{
			reflect.NewStore(s.nodes, node, nil),
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
			reflect.NewStore(s.pvcs, volume, nil),
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
		{
			// Warning-only server side: Normal events churn constantly and
			// would rebroadcast a frame per pod tick.
			reflect.NewStore(s.events, alert, nil),
			&corev1.Event{},
			reflect.TrackHealth(&cache.ListWatch{
				ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
					o.FieldSelector = "type=Warning"
					return cs.CoreV1().Events(metav1.NamespaceAll).List(ctx, o)
				},
				WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
					o.FieldSelector = "type=Warning"
					return cs.CoreV1().Events(metav1.NamespaceAll).Watch(ctx, o)
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
// and stops when ctx is cancelled. Returns immediately; early frames may be
// partial until the initial LISTs land, and the hub rebroadcasts as they do.
func (s *State) Run(ctx context.Context) {
	for _, spec := range s.specs {
		r := cache.NewReflector(spec.lw, spec.expected, spec.store, 0)
		go r.Run(ctx.Done())
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
	var smartAlerts []model.Alert
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
				Smart:      d.Smart,
			}
			if d.Size != nil {
				disk.SizeBytes = d.Size.Value()
			}
			if d.TempCelsius != nil {
				t := int(*d.TempCelsius)
				disk.TempCelsius = &t
			}
			disk.PowerOnHours = d.PowerOnHours
			disksByNode[inv.Name] = append(disksByNode[inv.Name], disk)
			if d.Smart == "Failed" {
				// Synthetic alert: smartctl has no event path into the
				// cluster, and a failing disk must lead the trouble feed.
				// LastSeen stays empty so the frame does not churn on
				// every inventory tick.
				smartAlerts = append(smartAlerts, model.Alert{
					Object:  "Disk/" + inv.Name + ":" + d.Path,
					Reason:  "SmartFailed",
					Message: "SMART health check failed on " + inv.Name + " (" + d.Path + "); replace the disk",
					Count:   1,
				})
			}
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
	for _, u := range reflect.List(s.targets) {
		var target storagev1alpha1.Target
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &target); err != nil {
			continue
		}
		snap.Targets = append(snap.Targets, targetModel(&target))
	}
	for _, u := range reflect.List(s.snapshots) {
		snap.Snapshots = append(snap.Snapshots, snapshotModel(u))
	}
	for _, obj := range s.events.List() {
		ev, ok := obj.(*corev1.Event)
		if !ok {
			continue
		}
		snap.Alerts = append(snap.Alerts, alertModel(ev))
	}
	// Newest first, capped: the feed is a trouble log, not an archive.
	sort.Slice(snap.Alerts, func(i, j int) bool {
		if snap.Alerts[i].LastSeen != snap.Alerts[j].LastSeen {
			return snap.Alerts[i].LastSeen > snap.Alerts[j].LastSeen
		}
		a, b := snap.Alerts[i], snap.Alerts[j]
		return a.Namespace+a.Object+a.Reason < b.Namespace+b.Object+b.Reason
	})
	if len(snap.Alerts) > 100 {
		snap.Alerts = snap.Alerts[:100]
	}
	// Failing disks lead the feed regardless of event timestamps.
	sort.Slice(smartAlerts, func(i, j int) bool { return smartAlerts[i].Object < smartAlerts[j].Object })
	snap.Alerts = append(smartAlerts, snap.Alerts...)
	sort.Slice(snap.Targets, func(i, j int) bool {
		a, b := snap.Targets[i], snap.Targets[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	sort.Slice(snap.Snapshots, func(i, j int) bool {
		a, b := snap.Snapshots[i], snap.Snapshots[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
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
	if p.Status.RebuildPercent != nil {
		pct := int(*p.Status.RebuildPercent)
		out.RebuildPercent = &pct
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

func targetModel(t *storagev1alpha1.Target) model.Target {
	out := model.Target{
		Namespace:  t.Namespace,
		Name:       t.Name,
		IQN:        t.Status.IQN,
		VIP:        t.Spec.VIP,
		ActiveNode: t.Status.ActiveNode,
		Sessions:   t.Status.Sessions,
		State:      t.Status.State,
	}
	if out.State == "" {
		out.State = "Pending"
	}
	resolved := map[int32]string{}
	for _, l := range t.Status.LUNs {
		resolved[l.ID] = l.Device
	}
	for _, l := range t.Spec.LUNs {
		out.LUNs = append(out.LUNs, model.TargetLUN{ID: l.ID, Claim: l.ClaimName, Device: resolved[l.ID]})
	}
	for _, c := range t.Status.Conditions {
		if c.Type == storagev1alpha1.ConditionAvailable {
			out.Available = c.Status == metav1.ConditionTrue
			out.Reason = c.Reason
		}
	}
	return out
}

// snapshotModel reads the external-snapshotter CR without importing its
// module: the four fields the UI shows do not justify a dependency.
func snapshotModel(u *unstructured.Unstructured) model.VolumeSnapshot {
	out := model.VolumeSnapshot{Namespace: u.GetNamespace(), Name: u.GetName()}
	if src, ok, _ := unstructured.NestedString(u.Object, "spec", "source", "persistentVolumeClaimName"); ok {
		out.Source = src
	}
	if ready, ok, _ := unstructured.NestedBool(u.Object, "status", "readyToUse"); ok {
		out.Ready = ready
	}
	if size, ok, _ := unstructured.NestedString(u.Object, "status", "restoreSize"); ok {
		if q, err := apiresource.ParseQuantity(size); err == nil {
			out.SizeBytes = q.Value()
		}
	}
	if t, ok, _ := unstructured.NestedString(u.Object, "status", "creationTime"); ok {
		out.CreatedAt = t
	}
	return out
}

func alertModel(ev *corev1.Event) model.Alert {
	out := model.Alert{
		Namespace: ev.Namespace,
		Object:    ev.InvolvedObject.Kind + "/" + ev.InvolvedObject.Name,
		Reason:    ev.Reason,
		Message:   ev.Message,
		Count:     ev.Count,
	}
	// events.k8s.io-originated objects leave the legacy fields empty.
	last := ev.LastTimestamp.Time
	if last.IsZero() {
		last = ev.EventTime.Time
	}
	if ev.Series != nil {
		last = ev.Series.LastObservedTime.Time
		out.Count = ev.Series.Count
	}
	if !last.IsZero() {
		out.LastSeen = last.UTC().Format(time.RFC3339)
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

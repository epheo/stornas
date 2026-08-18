// Package api holds the mutation endpoints: thin translators from JSON
// requests to CRs and PVCs. All validation authority stays in the CRDs
// (CEL, enums, schemas) so the UI and kubectl are rejected identically;
// handlers only shape the object and surface the apiserver's answer.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	lapi "github.com/LINBIT/golinstor/client"

	"github.com/epheo/stornas/internal/auth"
	"github.com/epheo/stornas/internal/tasks"
	storagev1alpha1 "github.com/epheo/stornas/operator/api/v1alpha1"
)

var (
	poolGVR   = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "storagepools"}
	shareGVR  = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "shares"}
	targetGVR = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "targets"}
	userGVR   = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "localusers"}
	snapGVR   = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
)

// API creates appliance objects. Namespace scopes everything namespaced:
// appliance-managed volumes and shares live in the system namespace.
// Linstor is nil when LINSTOR_URL is unset; endpoints needing it 501.
type API struct {
	Dyn       dynamic.Interface
	CS        kubernetes.Interface
	Namespace string
	Tasks     *tasks.Feed
	Linstor   *lapi.Client
}

// record adds one row to the activity feed, attributed to the session
// identity Require put on the context. Refused mutations record too, as
// not-OK: the audit trail answers "who tried", not just "who succeeded".
func (a *API) record(r *http.Request, verb, name string, err error) {
	if a.Tasks == nil {
		return
	}
	a.Tasks.RecordOp(tasks.Op{
		Verb:      verb,
		Namespace: a.Namespace,
		Name:      name,
		By:        auth.FromContext(r.Context()).Name,
		OK:        err == nil,
		At:        time.Now().UTC(),
	})
}

type poolRequest struct {
	Name    string   `json:"name"`
	Node    string   `json:"node"`
	Devices []string `json:"devices"`
	Raid    string   `json:"raid"`
}

func (a *API) CreatePool(w http.ResponseWriter, r *http.Request) {
	var req poolRequest
	if !decode(w, r, &req) {
		return
	}
	pool := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.stornas.io/v1alpha1",
		"kind":       "StoragePool",
		"metadata":   map[string]any{"name": req.Name},
		"spec": map[string]any{
			"node":    req.Node,
			"devices": toAny(req.Devices),
			"raid":    orDefault(req.Raid, "none"),
		},
	}}
	created, err := a.Dyn.Resource(poolGVR).Create(r.Context(), pool, metav1.CreateOptions{})
	a.record(r, "create pool", req.Name, err)
	respond(w, created, err)
}

type replaceRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// ReplacePoolDevice swaps one member of spec.devices; the agent converges
// the VG (extend, repair or evacuate, reduce). The CRD's same-size CEL
// rule keeps this a swap, never a grow or shrink.
// DeletePool refuses while LINSTOR still holds resources on the pool's
// node: without the check the CR hangs in terminating and the user
// never learns why. v1 has one pool per node, so node scope is pool
// scope. The operator's finalizer chain (LINSTOR deregister, agent host
// wipe) remains the real guard.
func (a *API) DeletePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	u, err := a.Dyn.Resource(poolGVR).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		respondDelete(w, err)
		return
	}
	node, _, _ := unstructured.NestedString(u.Object, "spec", "node")
	if a.Linstor != nil {
		// Fail closed: without the view we cannot prove the pool is idle,
		// and deleting blind is the data-loss path this guard exists for.
		view, verr := a.Linstor.Resources.GetResourceView(r.Context())
		if verr != nil {
			http.Error(w, "cannot verify the pool is unused, linstor: "+verr.Error(), http.StatusBadGateway)
			return
		}
		for _, res := range view {
			if res.NodeName != node {
				continue
			}
			// A diskless replica (tiebreaker) lives in the diskless pool
			// and holds no data here.
			if slices.Contains(res.Flags, "DISKLESS") {
				continue
			}
			http.Error(w, "pool still backs volumes; delete them first", http.StatusConflict)
			return
		}
	}
	derr := a.Dyn.Resource(poolGVR).Delete(r.Context(), name, metav1.DeleteOptions{})
	a.record(r, "delete pool", name, derr)
	respondDelete(w, derr)
}

func (a *API) ReplacePoolDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req replaceRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Old == "" || req.New == "" {
		http.Error(w, "old and new device paths required", http.StatusBadRequest)
		return
	}
	pool, err := a.Dyn.Resource(poolGVR).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		a.record(r, "replace disk", name, err)
		respondDelete(w, err)
		return
	}
	devices, _, _ := unstructured.NestedStringSlice(pool.Object, "spec", "devices")
	replaced := false
	for i, dev := range devices {
		if dev == req.Old {
			devices[i] = req.New
			replaced = true
		}
	}
	if !replaced {
		http.Error(w, req.Old+" is not a member of pool "+name, http.StatusConflict)
		return
	}
	_ = unstructured.SetNestedStringSlice(pool.Object, devices, "spec", "devices")
	_, uerr := a.Dyn.Resource(poolGVR).Update(r.Context(), pool, metav1.UpdateOptions{})
	a.record(r, "replace disk", name, uerr)
	respondDelete(w, uerr)
}

type volumeRequest struct {
	Name         string `json:"name"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass"`
	Block        bool   `json:"block"`
	// FromSnapshot restores: the new volume starts as a copy of this
	// VolumeSnapshot instead of empty.
	FromSnapshot string `json:"fromSnapshot"`
}

func (a *API) CreateVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.FromSnapshot != "" {
		snap, err := a.Dyn.Resource(snapGVR).Namespace(a.Namespace).Get(r.Context(), req.FromSnapshot, metav1.GetOptions{})
		if err != nil {
			http.Error(w, "snapshot: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Size == "" {
			// Restores default to the snapshot's own size.
			restore, ok, _ := unstructured.NestedString(snap.Object, "status", "restoreSize")
			if !ok {
				http.Error(w, "snapshot has no restoreSize yet; pass an explicit size", http.StatusBadRequest)
				return
			}
			req.Size = restore
		}
		// Restores inherit class and mode from the snapshot's source PVC:
		// falling to the cluster default would silently downgrade a
		// replicated volume to local, or mount a block image as a fs.
		if src, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "persistentVolumeClaimName"); src != "" {
			pvc, err := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Get(r.Context(), src, metav1.GetOptions{})
			switch {
			case err == nil:
				if req.StorageClass == "" && pvc.Spec.StorageClassName != nil {
					req.StorageClass = *pvc.Spec.StorageClassName
				}
				if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
					req.Block = true
				}
			case req.StorageClass == "":
				// Fail closed: a source gone or unreadable means the
				// inheritance cannot happen, and proceeding is exactly the
				// silent downgrade above. An explicit class is the caller
				// taking that responsibility, block flag included.
				http.Error(w, "snapshot source PVC "+src+" is not readable ("+err.Error()+
					"); pass storageClass (and block) explicitly", http.StatusConflict)
				return
			}
		}
	}
	size, err := resource.ParseQuantity(req.Size)
	if err != nil {
		http.Error(w, "invalid size: "+err.Error(), http.StatusBadRequest)
		return
	}
	mode := corev1.PersistentVolumeFilesystem
	if req.Block {
		mode = corev1.PersistentVolumeBlock
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name, Namespace: a.Namespace,
			// UI volumes are served by nfsd or LIO, never a pod; the
			// declaration lets the binder complete WFFC without guessing.
			Annotations: map[string]string{
				storagev1alpha1.ConsumerAnnotation: storagev1alpha1.ConsumerHost,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  &mode,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if req.StorageClass != "" {
		pvc.Spec.StorageClassName = &req.StorageClass
	}
	if req.FromSnapshot != "" {
		group := "snapshot.storage.k8s.io"
		pvc.Spec.DataSource = &corev1.TypedLocalObjectReference{
			APIGroup: &group,
			Kind:     "VolumeSnapshot",
			Name:     req.FromSnapshot,
		}
	}
	created, cerr := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Create(r.Context(), pvc, metav1.CreateOptions{})
	verb := "create volume"
	if req.FromSnapshot != "" {
		verb = "restore volume"
	}
	a.record(r, verb, req.Name, cerr)
	respond(w, created, cerr)
}

// DeleteVolume refuses while a Share or Target still references the claim:
// deleting the PVC under an export strands the client, and the CRDs cannot
// express cross-object liveness.
func (a *API) DeleteVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if ref, err := a.claimReferenced(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if ref != "" {
		a.record(r, "delete volume", name, errInUse)
		http.Error(w, "volume is in use by "+ref, http.StatusConflict)
		return
	}
	err := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Delete(r.Context(), name, metav1.DeleteOptions{})
	a.record(r, "delete volume", name, err)
	respondDelete(w, err)
}

// errInUse marks a refused mutation in the activity feed; the HTTP answer
// carries the real message.
var errInUse = errors.New("in use")

func (a *API) claimReferenced(ctx context.Context, claim string) (string, error) {
	shares, err := a.Dyn.Resource(shareGVR).Namespace(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, s := range shares.Items {
		if c, _, _ := unstructured.NestedString(s.Object, "spec", "claimName"); c == claim {
			return "share " + s.GetName(), nil
		}
	}
	targets, err := a.Dyn.Resource(targetGVR).Namespace(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, t := range targets.Items {
		luns, _, _ := unstructured.NestedSlice(t.Object, "spec", "luns")
		for _, l := range luns {
			m, ok := l.(map[string]any)
			if ok && m["claimName"] == claim {
				return "target " + t.GetName(), nil
			}
		}
	}
	return "", nil
}

type resolveSplitRequest struct {
	Survivor string `json:"survivor"`
}

// ResolveSplitBrain is the failure matrix's pick-survivor flow: every
// diskful replica except the survivor is deleted from LINSTOR and
// autoplaced back, discarding the losing side's writes and resyncing in
// full from the survivor. DRBD cannot merge diverged data; picking is
// the only honest resolution.
func (a *API) ResolveSplitBrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req resolveSplitRequest
	if !decode(w, r, &req) {
		return
	}
	if a.Linstor == nil {
		http.Error(w, "LINSTOR is not configured", http.StatusNotImplemented)
		return
	}
	if req.Survivor == "" {
		http.Error(w, "survivor node required", http.StatusBadRequest)
		return
	}
	pvc, err := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		respondDelete(w, err)
		return
	}
	res := pvc.Spec.VolumeName
	if res == "" {
		http.Error(w, "volume has no bound PV yet", http.StatusConflict)
		return
	}
	resources, err := a.Linstor.Resources.GetAll(r.Context(), res)
	if err != nil {
		http.Error(w, "linstor: "+err.Error(), http.StatusBadGateway)
		return
	}
	diskful := 0
	survivorSeen := false
	var victims []string
	for _, rsc := range resources {
		diskless := false
		for _, f := range rsc.Flags {
			if f == "DISKLESS" {
				diskless = true
			}
		}
		if diskless {
			continue
		}
		diskful++
		if rsc.NodeName == req.Survivor {
			survivorSeen = true
		} else {
			victims = append(victims, rsc.NodeName)
		}
	}
	if !survivorSeen {
		http.Error(w, req.Survivor+" holds no replica of "+name, http.StatusConflict)
		return
	}
	for _, node := range victims {
		if err := a.Linstor.Resources.Delete(r.Context(), res, node); err != nil {
			a.record(r, "resolve split-brain", name, err)
			http.Error(w, "delete replica on "+node+": "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	aerr := a.Linstor.Resources.Autoplace(r.Context(), res, lapi.AutoPlaceRequest{
		SelectFilter: lapi.AutoSelectFilter{PlaceCount: int32(diskful)},
	})
	a.record(r, "resolve split-brain", name, aerr)
	if aerr != nil {
		http.Error(w, "replicas discarded but autoplace failed, retry resolves it: "+aerr.Error(), http.StatusBadGateway)
		return
	}
	respondDelete(w, nil)
}

type resizeRequest struct {
	Size string `json:"size"`
}

func (a *API) ResizeVolume(w http.ResponseWriter, r *http.Request) {
	var req resizeRequest
	if !decode(w, r, &req) {
		return
	}
	size, err := resource.ParseQuantity(req.Size)
	if err != nil {
		http.Error(w, "invalid size: "+err.Error(), http.StatusBadRequest)
		return
	}
	patch := []byte(`{"spec":{"resources":{"requests":{"storage":"` + size.String() + `"}}}}`)
	_, perr := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Patch(
		r.Context(), r.PathValue("name"), types.MergePatchType, patch, metav1.PatchOptions{})
	a.record(r, "resize volume", r.PathValue("name"), perr)
	respondDelete(w, perr)
}

func (a *API) DeleteShare(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(shareGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
	a.record(r, "delete share", r.PathValue("name"), err)
	respondDelete(w, err)
}

type targetRequest struct {
	Name string `json:"name"`
	VIP  string `json:"vip"`
	LUNs []struct {
		ID    int32  `json:"id"`
		Claim string `json:"claim"`
	} `json:"luns"`
	Initiators []string `json:"initiators"`
}

func (a *API) CreateTarget(w http.ResponseWriter, r *http.Request) {
	var req targetRequest
	if !decode(w, r, &req) {
		return
	}
	luns := make([]any, 0, len(req.LUNs))
	for _, l := range req.LUNs {
		luns = append(luns, map[string]any{"id": int64(l.ID), "claimName": l.Claim})
	}
	spec := map[string]any{"luns": luns}
	if req.VIP != "" {
		spec["vip"] = req.VIP
	}
	if len(req.Initiators) > 0 {
		inits := make([]any, 0, len(req.Initiators))
		for _, iqn := range req.Initiators {
			inits = append(inits, map[string]any{"iqn": iqn})
		}
		spec["initiators"] = inits
	}
	target := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.stornas.io/v1alpha1",
		"kind":       "Target",
		"metadata":   map[string]any{"name": req.Name, "namespace": a.Namespace},
		"spec":       spec,
	}}
	created, err := a.Dyn.Resource(targetGVR).Namespace(a.Namespace).Create(r.Context(), target, metav1.CreateOptions{})
	a.record(r, "create target", req.Name, err)
	respond(w, created, err)
}

func (a *API) DeleteTarget(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(targetGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
	a.record(r, "delete target", r.PathValue("name"), err)
	respondDelete(w, err)
}

type snapshotRequest struct {
	Name   string `json:"name"`
	Volume string `json:"volume"`
}

func (a *API) CreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotRequest
	if !decode(w, r, &req) {
		return
	}
	snap := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   map[string]any{"name": req.Name, "namespace": a.Namespace},
		"spec": map[string]any{
			"volumeSnapshotClassName": "stornas",
			"source":                  map[string]any{"persistentVolumeClaimName": req.Volume},
		},
	}}
	created, err := a.Dyn.Resource(snapGVR).Namespace(a.Namespace).Create(r.Context(), snap, metav1.CreateOptions{})
	a.record(r, "create snapshot", req.Name, err)
	respond(w, created, err)
}

func (a *API) DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(snapGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
	a.record(r, "delete snapshot", r.PathValue("name"), err)
	respondDelete(w, err)
}

type shareRequest struct {
	Name       string   `json:"name"`
	Claim      string   `json:"claim"`
	NFSClients []string `json:"nfsClients"`
	SMB        bool     `json:"smb"`
	ValidUsers []string `json:"validUsers"`
}

// nfsClientRE is one /etc/exports client entry: host or subnet, then an
// optional parenthesised option list. A malformed entry would land in
// the host's exports file and take every share on the node down with a
// syntax error, so it is refused here.
var nfsClientRE = regexp.MustCompile(`^[^\s()]+(\([^\s()]*\))?$`)

func (a *API) CreateShare(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if !decode(w, r, &req) {
		return
	}
	for _, c := range req.NFSClients {
		if !nfsClientRE.MatchString(c) {
			http.Error(w, "invalid NFS client entry: "+c, http.StatusBadRequest)
			return
		}
	}
	spec := map[string]any{"claimName": req.Claim}
	if len(req.NFSClients) > 0 {
		spec["nfs"] = map[string]any{"clients": toAny(req.NFSClients)}
	}
	if req.SMB {
		smb := map[string]any{}
		if len(req.ValidUsers) > 0 {
			smb["validUsers"] = toAny(req.ValidUsers)
		}
		spec["smb"] = smb
	}
	share := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.stornas.io/v1alpha1",
		"kind":       "Share",
		"metadata":   map[string]any{"name": req.Name, "namespace": a.Namespace},
		"spec":       spec,
	}}
	created, err := a.Dyn.Resource(shareGVR).Namespace(a.Namespace).Create(r.Context(), share, metav1.CreateOptions{})
	a.record(r, "create share", req.Name, err)
	respond(w, created, err)
}

type userRequest struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
	SMB      bool   `json:"smb"`
}

// CreateUser makes the password Secret first: a LocalUser with a dangling
// ref would let login fail closed but confuse the agent's smb reconciler.
func (a *API) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if !decode(w, r, &req) {
		return
	}
	// Same floor as the self-service change; the password lives in a Secret,
	// so CRD validation cannot enforce it.
	if len(req.Password) < 8 {
		http.Error(w, "password needs at least 8 characters", http.StatusBadRequest)
		return
	}
	secretName := req.Name + "-password"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: a.Namespace},
		StringData: map[string]string{"password": req.Password},
	}
	if _, err := a.CS.CoreV1().Secrets(a.Namespace).Create(r.Context(), secret, metav1.CreateOptions{}); err != nil {
		respond(w, nil, err)
		return
	}
	user := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.stornas.io/v1alpha1",
		"kind":       "LocalUser",
		"metadata":   map[string]any{"name": req.Name, "namespace": a.Namespace},
		"spec": map[string]any{
			"role":              orDefault(req.Role, "viewer"),
			"smb":               req.SMB,
			"passwordSecretRef": secretName,
		},
	}}
	created, err := a.Dyn.Resource(userGVR).Namespace(a.Namespace).Create(r.Context(), user, metav1.CreateOptions{})
	if err != nil {
		// Roll the Secret back so a retry with a fixed spec starts clean.
		_ = a.CS.CoreV1().Secrets(a.Namespace).Delete(r.Context(), secretName, metav1.DeleteOptions{})
	}
	a.record(r, "create user", req.Name, err)
	respond(w, created, err)
}

func (a *API) DeleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "admin" {
		http.Error(w, "the admin user cannot be deleted", http.StatusConflict)
		return
	}
	u, err := a.Dyn.Resource(userGVR).Namespace(a.Namespace).Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		respondDelete(w, err)
		return
	}
	if ref, _, _ := unstructured.NestedString(u.Object, "spec", "passwordSecretRef"); ref != "" {
		_ = a.CS.CoreV1().Secrets(a.Namespace).Delete(r.Context(), ref, metav1.DeleteOptions{})
	}
	derr := a.Dyn.Resource(userGVR).Namespace(a.Namespace).Delete(r.Context(), name, metav1.DeleteOptions{})
	a.record(r, "delete user", name, derr)
	respondDelete(w, derr)
}

// ListUsers returns identity only; password material never leaves the
// Secret.
func (a *API) ListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := a.Dyn.Resource(userGVR).Namespace(a.Namespace).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type user struct {
		Name string `json:"name"`
		Role string `json:"role"`
		SMB  bool   `json:"smb"`
	}
	users := make([]user, 0, len(list.Items))
	for _, item := range list.Items {
		role, _, _ := unstructured.NestedString(item.Object, "spec", "role")
		smb, _, _ := unstructured.NestedBool(item.Object, "spec", "smb")
		users = append(users, user{Name: item.GetName(), Role: role, SMB: smb})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(users)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// respond maps apiserver rejections onto client-visible statuses: CRD
// validation is the single source of truth, so its messages pass through.
func respond(w http.ResponseWriter, created any, err error) {
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "created"})
		_ = created
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case apierrors.IsAlreadyExists(err):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// respondDelete maps mutation errors for delete/patch verbs: NotFound is
// the client's stale view, not a server fault.
func respondDelete(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	case apierrors.IsNotFound(err):
		http.Error(w, err.Error(), http.StatusNotFound)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsForbidden(err):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

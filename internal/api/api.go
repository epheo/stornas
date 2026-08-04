// Package api holds the mutation endpoints: thin translators from JSON
// requests to CRs and PVCs. All validation authority stays in the CRDs
// (CEL, enums, schemas) so the UI and kubectl are rejected identically;
// handlers only shape the object and surface the apiserver's answer.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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
type API struct {
	Dyn       dynamic.Interface
	CS        kubernetes.Interface
	Namespace string
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
	respond(w, created, err)
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
	if req.Size == "" && req.FromSnapshot != "" {
		// Restores default to the snapshot's own size.
		snap, err := a.Dyn.Resource(snapGVR).Namespace(a.Namespace).Get(r.Context(), req.FromSnapshot, metav1.GetOptions{})
		if err != nil {
			http.Error(w, "snapshot: "+err.Error(), http.StatusBadRequest)
			return
		}
		restore, ok, _ := unstructured.NestedString(snap.Object, "status", "restoreSize")
		if !ok {
			http.Error(w, "snapshot has no restoreSize yet; pass an explicit size", http.StatusBadRequest)
			return
		}
		req.Size = restore
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
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: a.Namespace},
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
		http.Error(w, "volume is in use by "+ref, http.StatusConflict)
		return
	}
	err := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Delete(r.Context(), name, metav1.DeleteOptions{})
	respondDelete(w, err)
}

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
	respondDelete(w, perr)
}

func (a *API) DeleteShare(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(shareGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
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
	respond(w, created, err)
}

func (a *API) DeleteTarget(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(targetGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
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
	respond(w, created, err)
}

func (a *API) DeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	err := a.Dyn.Resource(snapGVR).Namespace(a.Namespace).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
	respondDelete(w, err)
}

type shareRequest struct {
	Name       string   `json:"name"`
	Claim      string   `json:"claim"`
	NFSClients []string `json:"nfsClients"`
	SMB        bool     `json:"smb"`
	ValidUsers []string `json:"validUsers"`
}

func (a *API) CreateShare(w http.ResponseWriter, r *http.Request) {
	var req shareRequest
	if !decode(w, r, &req) {
		return
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
	if req.Password == "" {
		http.Error(w, "password required", http.StatusBadRequest)
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
	respondDelete(w, a.Dyn.Resource(userGVR).Namespace(a.Namespace).Delete(r.Context(), name, metav1.DeleteOptions{}))
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

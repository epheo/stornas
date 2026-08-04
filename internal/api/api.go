// Package api holds the mutation endpoints: thin translators from JSON
// requests to CRs and PVCs. All validation authority stays in the CRDs
// (CEL, enums, schemas) so the UI and kubectl are rejected identically;
// handlers only shape the object and surface the apiserver's answer.
package api

import (
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	poolGVR  = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "storagepools"}
	shareGVR = schema.GroupVersionResource{Group: "storage.stornas.io", Version: "v1alpha1", Resource: "shares"}
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
}

func (a *API) CreateVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if !decode(w, r, &req) {
		return
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
	created, cerr := a.CS.CoreV1().PersistentVolumeClaims(a.Namespace).Create(r.Context(), pvc, metav1.CreateOptions{})
	respond(w, created, cerr)
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

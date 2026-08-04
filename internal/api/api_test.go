package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func newAPI() *API {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		poolGVR:  "StoragePoolList",
		shareGVR: "ShareList",
	}
	return &API{
		Dyn:       dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		CS:        k8sfake.NewClientset(),
		Namespace: "stornas-system",
	}
}

func TestCreatePool(t *testing.T) {
	a := newAPI()
	body := `{"name":"tank","node":"node-a","devices":["/dev/disk/by-id/wwn-0xabc"],"raid":"raid1"}`
	w := httptest.NewRecorder()
	a.CreatePool(w, httptest.NewRequest("POST", "/api/v1/pools", strings.NewReader(body)))
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := a.Dyn.Resource(poolGVR).Get(context.Background(), "tank", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node, _, _ := unstructured.NestedString(got.Object, "spec", "node")
	raid, _, _ := unstructured.NestedString(got.Object, "spec", "raid")
	if node != "node-a" || raid != "raid1" {
		t.Fatalf("spec = %v", got.Object["spec"])
	}
}

func TestCreateShareShapesProtocols(t *testing.T) {
	a := newAPI()
	body := `{"name":"media","claim":"media","nfsClients":["10.0.0.0/8(rw)"],"smb":true,"validUsers":["alice"]}`
	w := httptest.NewRecorder()
	a.CreateShare(w, httptest.NewRequest("POST", "/api/v1/shares", strings.NewReader(body)))
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := a.Dyn.Resource(shareGVR).Namespace("stornas-system").Get(context.Background(), "media", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	clients, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "nfs", "clients")
	users, _, _ := unstructured.NestedStringSlice(got.Object, "spec", "smb", "validUsers")
	if len(clients) != 1 || len(users) != 1 {
		t.Fatalf("spec = %v", got.Object["spec"])
	}
}

func TestCreateVolumeRejectsBadSize(t *testing.T) {
	a := newAPI()
	w := httptest.NewRecorder()
	a.CreateVolume(w, httptest.NewRequest("POST", "/api/v1/volumes", strings.NewReader(`{"name":"v","size":"lots"}`)))
	if w.Code != 400 {
		t.Fatalf("code=%d", w.Code)
	}
}

func TestCreateVolumeBlockMode(t *testing.T) {
	a := newAPI()
	body := `{"name":"disk0","size":"10Gi","storageClass":"stornas-replicated","block":true}`
	w := httptest.NewRecorder()
	a.CreateVolume(w, httptest.NewRequest("POST", "/api/v1/volumes", strings.NewReader(body)))
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	pvc, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Get(context.Background(), "disk0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *pvc.Spec.StorageClassName != "stornas-replicated" || string(*pvc.Spec.VolumeMode) != "Block" {
		t.Fatalf("pvc = %+v", pvc.Spec)
	}
}

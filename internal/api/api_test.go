package api

import (
	"context"
	"net/http"
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
		poolGVR:   "StoragePoolList",
		shareGVR:  "ShareList",
		targetGVR: "TargetList",
		snapGVR:   "VolumeSnapshotList",
	}
	return &API{
		Dyn:       dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		CS:        k8sfake.NewClientset(),
		Namespace: "stornas-system",
	}
}

func doReq(t *testing.T, fn func(w http.ResponseWriter, r *http.Request), method, path, body string, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range pathValues {
		r.SetPathValue(k, v)
	}
	fn(w, r)
	return w
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

func TestDeleteVolumeRefusedWhileShared(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi"}`, nil)
	doReq(t, a.CreateShare, "POST", "/api/v1/shares", `{"name":"media","claim":"media","smb":true}`, nil)
	w := doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/media", "", map[string]string{"name": "media"})
	if w.Code != 409 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	doReq(t, a.DeleteShare, "DELETE", "/api/v1/shares/media", "", map[string]string{"name": "media"})
	w = doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/media", "", map[string]string{"name": "media"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Get(context.Background(), "media", metav1.GetOptions{}); err == nil {
		t.Fatal("pvc still exists after delete")
	}
}

func TestDeleteVolumeRefusedWhileLUN(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"disk0","size":"1Gi","block":true}`, nil)
	doReq(t, a.CreateTarget, "POST", "/api/v1/targets", `{"name":"vmstore","luns":[{"id":0,"claim":"disk0"}]}`, nil)
	w := doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/disk0", "", map[string]string{"name": "disk0"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "target vmstore") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestResizeVolume(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"v","size":"1Gi"}`, nil)
	w := doReq(t, a.ResizeVolume, "POST", "/api/v1/volumes/v/resize", `{"size":"2Gi"}`, map[string]string{"name": "v"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	w = doReq(t, a.ResizeVolume, "POST", "/api/v1/volumes/v/resize", `{"size":"nope"}`, map[string]string{"name": "v"})
	if w.Code != 400 {
		t.Fatalf("bad size accepted: code=%d", w.Code)
	}
}

func TestCreateTargetShapesSpec(t *testing.T) {
	a := newAPI()
	body := `{"name":"vmstore","vip":"10.0.0.50/24","luns":[{"id":0,"claim":"disk0"}],"initiators":["iqn.1993-08.org.debian:01:abc"]}`
	w := doReq(t, a.CreateTarget, "POST", "/api/v1/targets", body, nil)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := a.Dyn.Resource(targetGVR).Namespace("stornas-system").Get(context.Background(), "vmstore", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vip, _, _ := unstructured.NestedString(got.Object, "spec", "vip")
	luns, _, _ := unstructured.NestedSlice(got.Object, "spec", "luns")
	inits, _, _ := unstructured.NestedSlice(got.Object, "spec", "initiators")
	if vip != "10.0.0.50/24" || len(luns) != 1 || len(inits) != 1 {
		t.Fatalf("spec = %v", got.Object["spec"])
	}
}

func TestSnapshotLifecycleAndRestore(t *testing.T) {
	a := newAPI()
	w := doReq(t, a.CreateSnapshot, "POST", "/api/v1/snapshots", `{"name":"s1","volume":"media"}`, nil)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := a.Dyn.Resource(snapGVR).Namespace("stornas-system").Get(context.Background(), "s1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	class, _, _ := unstructured.NestedString(got.Object, "spec", "volumeSnapshotClassName")
	src, _, _ := unstructured.NestedString(got.Object, "spec", "source", "persistentVolumeClaimName")
	if class != "stornas" || src != "media" {
		t.Fatalf("spec = %v", got.Object["spec"])
	}

	// Restore without a size fails until restoreSize exists, then defaults.
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1"}`, nil)
	if w.Code != 400 {
		t.Fatalf("expected 400 before restoreSize, got %d", w.Code)
	}
	_ = unstructured.SetNestedField(got.Object, "3Gi", "status", "restoreSize")
	if _, err := a.Dyn.Resource(snapGVR).Namespace("stornas-system").Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1"}`, nil)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	pvc, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Get(context.Background(), "r1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Name != "s1" {
		t.Fatalf("dataSource = %+v", pvc.Spec.DataSource)
	}
	if q := pvc.Spec.Resources.Requests["storage"]; q.String() != "3Gi" {
		t.Fatalf("size = %s", q.String())
	}

	w = doReq(t, a.DeleteSnapshot, "DELETE", "/api/v1/snapshots/s1", "", map[string]string{"name": "s1"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
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

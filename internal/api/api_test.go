package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

		rdrbdPoolGVR:   "PoolList",
		rdrbdVolumeGVR: "VolumeList",
	}
	var nodes []runtime.Object
	for _, n := range []string{"node1", "node2", "node3"} {
		nodes = append(nodes, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	return &API{
		Dyn:       dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		CS:        k8sfake.NewClientset(nodes...),
		Namespace: "stornas-system",
	}
}

// sys is the path values of a volume or snapshot route in the system
// namespace.
var sys = map[string]string{"namespace": "stornas-system"}

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
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi"}`, sys)
	doReq(t, a.CreateShare, "POST", "/api/v1/shares", `{"name":"media","claim":"media","smb":true}`, nil)
	w := doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/media", "", map[string]string{"namespace": "stornas-system", "name": "media"})
	if w.Code != 409 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	doReq(t, a.DeleteShare, "DELETE", "/api/v1/shares/media", "", map[string]string{"name": "media"})
	w = doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/media", "", map[string]string{"namespace": "stornas-system", "name": "media"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Get(context.Background(), "media", metav1.GetOptions{}); err == nil {
		t.Fatal("pvc still exists after delete")
	}
}

func TestDeleteVolumeRefusedWhileLUN(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"disk0","size":"1Gi","block":true}`, sys)
	doReq(t, a.CreateTarget, "POST", "/api/v1/targets", `{"name":"vmstore","luns":[{"id":0,"claim":"disk0"}]}`, nil)
	w := doReq(t, a.DeleteVolume, "DELETE", "/api/v1/volumes/disk0", "", map[string]string{"namespace": "stornas-system", "name": "disk0"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "target vmstore") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestResizeVolume(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"v","size":"1Gi"}`, sys)
	w := doReq(t, a.ResizeVolume, "POST", "/api/v1/volumes/v/resize", `{"size":"2Gi"}`, map[string]string{"namespace": "stornas-system", "name": "v"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	w = doReq(t, a.ResizeVolume, "POST", "/api/v1/volumes/v/resize", `{"size":"nope"}`, map[string]string{"namespace": "stornas-system", "name": "v"})
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
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi","storageClass":"stornas-replicated"}`, sys)
	w := doReq(t, a.CreateSnapshot, "POST", "/api/v1/snapshots", `{"name":"s1","volume":"media"}`, sys)
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
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1"}`, sys)
	if w.Code != 400 {
		t.Fatalf("expected 400 before restoreSize, got %d", w.Code)
	}
	_ = unstructured.SetNestedField(got.Object, "3Gi", "status", "restoreSize")
	if _, err := a.Dyn.Resource(snapGVR).Namespace("stornas-system").Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1"}`, sys)
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
	// The class rides over from the source PVC, never the cluster default.
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "stornas-replicated" {
		t.Fatalf("storageClass = %v", pvc.Spec.StorageClassName)
	}

	w = doReq(t, a.DeleteSnapshot, "DELETE", "/api/v1/snapshots/s1", "", map[string]string{"namespace": "stornas-system", "name": "s1"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

// A restore whose source PVC is gone must fail closed: guessing means
// the cluster default class and fs mode, the silent downgrade the
// inheritance exists to prevent. An explicit class opts out.
func TestRestoreFailsClosedWithoutSourcePVC(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi","storageClass":"stornas-replicated"}`, sys)
	doReq(t, a.CreateSnapshot, "POST", "/api/v1/snapshots", `{"name":"s1","volume":"media"}`, sys)
	got, err := a.Dyn.Resource(snapGVR).Namespace("stornas-system").Get(context.Background(), "s1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(got.Object, "1Gi", "status", "restoreSize")
	if _, err := a.Dyn.Resource(snapGVR).Namespace("stornas-system").Update(context.Background(), got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Delete(context.Background(), "media", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	w := doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1"}`, sys)
	if w.Code != 409 {
		t.Fatalf("expected 409 without source PVC, got %d: %s", w.Code, w.Body.String())
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r1","fromSnapshot":"s1","storageClass":"stornas-replicated"}`, sys)
	if w.Code != 201 {
		t.Fatalf("explicit class must pass, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserLifecycle(t *testing.T) {
	a := newAPI()
	w := doReq(t, a.CreateUser, "POST", "/api/v1/users", `{"name":"alice","password":"s3cretpass","role":"viewer","smb":true}`, nil)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	sec, err := a.CS.CoreV1().Secrets("stornas-system").Get(context.Background(), "alice-password", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sec.StringData["password"] != "s3cretpass" {
		t.Fatalf("secret = %v", sec.StringData)
	}
	got, err := a.Dyn.Resource(userGVR).Namespace("stornas-system").Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	smb, _, _ := unstructured.NestedBool(got.Object, "spec", "smb")
	ref, _, _ := unstructured.NestedString(got.Object, "spec", "passwordSecretRef")
	if !smb || ref != "alice-password" {
		t.Fatalf("spec = %v", got.Object["spec"])
	}

	w = doReq(t, a.CreateUser, "POST", "/api/v1/users", `{"name":"bob","role":"viewer"}`, nil)
	if w.Code != 400 {
		t.Fatalf("empty password accepted: code=%d", w.Code)
	}
	w = doReq(t, a.DeleteUser, "DELETE", "/api/v1/users/admin", "", map[string]string{"name": "admin"})
	if w.Code != 409 {
		t.Fatalf("admin delete allowed: code=%d", w.Code)
	}
	w = doReq(t, a.DeleteUser, "DELETE", "/api/v1/users/alice", "", map[string]string{"name": "alice"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := a.CS.CoreV1().Secrets("stornas-system").Get(context.Background(), "alice-password", metav1.GetOptions{}); err == nil {
		t.Fatal("password secret survived user delete")
	}
}

func TestCreateVolumeBlockMode(t *testing.T) {
	a := newAPI()
	body := `{"name":"disk0","size":"10Gi","storageClass":"stornas-replicated","block":true}`
	w := doReq(t, a.CreateVolume, "POST", "/api/v1/namespaces/stornas-system/volumes", body, sys)
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

// Two nodes cannot form DRBD quorum: replicated volumes are refused up
// front rather than created to suspend on the first peer loss.
func TestReplicatedNeedsThreeNodes(t *testing.T) {
	a := newAPI()
	if err := a.CS.CoreV1().Nodes().Delete(context.Background(), "node3", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	w := doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi","storageClass":"stornas-replicated"}`, sys)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "three nodes") {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi","storageClass":"stornas-local"}`, sys)
	if w.Code != 201 {
		t.Fatalf("local refused: code=%d body=%s", w.Code, w.Body.String())
	}
}

// One owner per disk is refused up front instead of surfacing later on
// the host; several pools of one tier on a node are fine.
func TestCreatePoolRefusesDeviceReuse(t *testing.T) {
	a := newAPI()
	ok := `{"name":"fast","node":"node1","devices":["/dev/disk/by-id/a"],"raid":"none","tier":"ssd"}`
	if w := doReq(t, a.CreatePool, "POST", "/api/v1/pools", ok, nil); w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	same := `{"name":"bulk","node":"node1","devices":["/dev/disk/by-id/a"],"tier":"hdd"}`
	if w := doReq(t, a.CreatePool, "POST", "/api/v1/pools", same, nil); w.Code != 409 {
		t.Fatalf("same device: code=%d body=%s", w.Code, w.Body.String())
	}
	for _, body := range []string{
		`{"name":"fast2","node":"node1","devices":["/dev/disk/by-id/c"],"tier":"ssd"}`,
		`{"name":"bulk","node":"node1","devices":["/dev/disk/by-id/b"],"tier":"hdd"}`,
		`{"name":"fast-b","node":"node2","devices":["/dev/disk/by-id/a"],"tier":"ssd"}`,
	} {
		if w := doReq(t, a.CreatePool, "POST", "/api/v1/pools", body, nil); w.Code != 201 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	}
}

// A pinned volume takes its pool's tier class and carries the pin for
// r-drbd's CSI controller; replicated tier classes need three nodes like
// the default.
func TestCreateVolumeOnPool(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreatePool, "POST", "/api/v1/pools", `{"name":"bulk","node":"node1","devices":["/dev/disk/by-id/b"],"tier":"hdd"}`, nil)
	w := doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"media","size":"1Gi","pool":"bulk"}`, sys)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	pvc, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Get(context.Background(), "media", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *pvc.Spec.StorageClassName != "stornas-local-hdd" || pvc.Annotations["r-drbd.io/pool"] != "bulk" {
		t.Fatalf("class=%s annotations=%v", *pvc.Spec.StorageClassName, pvc.Annotations)
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"m2","size":"1Gi","pool":"bulk","storageClass":"stornas-local"}`, sys)
	if w.Code != 400 {
		t.Fatalf("mismatched class accepted: code=%d", w.Code)
	}
	if err := a.CS.CoreV1().Nodes().Delete(context.Background(), "node3", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	w = doReq(t, a.CreateVolume, "POST", "/api/v1/volumes", `{"name":"r","size":"1Gi","storageClass":"stornas-replicated-hdd"}`, sys)
	if w.Code != 409 {
		t.Fatalf("replicated tier on two nodes: code=%d", w.Code)
	}
}

func createUnstructured(t *testing.T, a *API, gvr schema.GroupVersionResource, obj map[string]any) {
	t.Helper()
	if _, err := a.Dyn.Resource(gvr).Create(context.Background(), &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// The delete guard keys on the pool's own r-drbd Pool: volumes in another
// pool on the same node do not hold it.
func TestDeletePoolGuardIsPerPool(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreatePool, "POST", "/api/v1/pools", `{"name":"main","node":"node1","devices":["/dev/disk/by-id/a"]}`, nil)
	doReq(t, a.CreatePool, "POST", "/api/v1/pools", `{"name":"bulk","node":"node1","devices":["/dev/disk/by-id/b"]}`, nil)
	for name, vols := range map[string][]any{"main": {"pvc-1"}, "bulk": nil} {
		createUnstructured(t, a, rdrbdPoolGVR, map[string]any{
			"apiVersion": "r-drbd.io/v1alpha1", "kind": "Pool",
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"node": "node1", "tier": "default", "vg": "stornas-" + name, "thinPool": "thin"},
			"status":   map[string]any{"volumes": vols},
		})
	}
	w := doReq(t, a.DeletePool, "DELETE", "/api/v1/pools/main", "", map[string]string{"name": "main"})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "pvc-1") {
		t.Fatalf("busy pool deleted: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := doReq(t, a.DeletePool, "DELETE", "/api/v1/pools/bulk", "", map[string]string{"name": "bulk"}); w.Code != 200 {
		t.Fatalf("idle pool refused: code=%d body=%s", w.Code, w.Body.String())
	}
}

// A pool the operator never registered has nothing on it.
func TestDeletePoolWithoutBackingPool(t *testing.T) {
	a := newAPI()
	doReq(t, a.CreatePool, "POST", "/api/v1/pools", `{"name":"fresh","node":"node1","devices":["/dev/disk/by-id/a"]}`, nil)
	if w := doReq(t, a.DeletePool, "DELETE", "/api/v1/pools/fresh", "", map[string]string{"name": "fresh"}); w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestResolveSplitBrainSetsSurvivor(t *testing.T) {
	a := newAPI()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "stornas-system", Name: "media"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-1"},
	}
	if _, err := a.CS.CoreV1().PersistentVolumeClaims("stornas-system").Create(context.Background(), pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	replica := func(node, role string, id int64) any {
		return map[string]any{"node": node, "nodeId": id, "role": role}
	}
	vol := map[string]any{
		"apiVersion": "r-drbd.io/v1alpha1", "kind": "Volume",
		"metadata": map[string]any{"name": "pvc-1"},
		"spec": map[string]any{
			"sizeBytes": int64(1 << 30), "replicas": int64(2), "tier": "default",
			"assignment": map[string]any{"replicas": []any{
				replica("node1", "diskful", 0), replica("node2", "diskful", 1), replica("node3", "tiebreaker", 2),
			}},
		},
	}
	createUnstructured(t, a, rdrbdVolumeGVR, vol)
	path := map[string]string{"namespace": "stornas-system", "name": "media"}

	// No SplitBrain condition: discarding a copy would destroy good data.
	w := doReq(t, a.ResolveSplitBrain, "POST", "/", `{"survivor":"node2"}`, path)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "not in split brain") {
		t.Fatalf("healthy volume resolved: code=%d body=%s", w.Code, w.Body.String())
	}
	vol["status"] = map[string]any{"conditions": []any{map[string]any{
		"type": "SplitBrain", "status": "True", "reason": "Diverged", "message": "", "lastTransitionTime": "2026-10-15T00:00:00Z",
	}}}
	if _, err := a.Dyn.Resource(rdrbdVolumeGVR).Update(context.Background(), &unstructured.Unstructured{Object: vol}, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// The tiebreaker holds no data to keep.
	if w := doReq(t, a.ResolveSplitBrain, "POST", "/", `{"survivor":"node3"}`, path); w.Code != 409 {
		t.Fatalf("tiebreaker accepted: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := doReq(t, a.ResolveSplitBrain, "POST", "/", `{"survivor":"node2"}`, path); w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := a.Dyn.Resource(rdrbdVolumeGVR).Get(context.Background(), "pvc-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if survivor, _, _ := unstructured.NestedString(got.Object, "spec", "resolve", "survivor"); survivor != "node2" {
		t.Fatalf("survivor = %q", survivor)
	}
	// The merge patch leaves the controller's assignment alone.
	if reps, _, _ := unstructured.NestedSlice(got.Object, "spec", "assignment", "replicas"); len(reps) != 3 {
		t.Fatalf("assignment = %v", reps)
	}
}

// Issue 17: a volume outside the system namespace keeps every action in
// its own namespace, and a same-named system volume is never touched.
func TestVolumeActionsStayInTheClaimNamespace(t *testing.T) {
	a := newAPI()
	foreign := map[string]string{"namespace": "dotvirt"}
	for _, pv := range []map[string]string{sys, foreign} {
		w := doReq(t, a.CreateVolume, "POST", "/", `{"name":"drafts","size":"1Gi","storageClass":"stornas-local"}`, pv)
		if w.Code != 201 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	}
	w := doReq(t, a.CreateSnapshot, "POST", "/", `{"name":"s1","volume":"drafts"}`, foreign)
	if w.Code != 201 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := a.Dyn.Resource(snapGVR).Namespace("dotvirt").Get(context.Background(), "s1", metav1.GetOptions{}); err != nil {
		t.Fatalf("snapshot not beside its claim: %v", err)
	}
	w = doReq(t, a.ResizeVolume, "POST", "/", `{"size":"2Gi"}`, map[string]string{"namespace": "dotvirt", "name": "drafts"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	w = doReq(t, a.DeleteVolume, "DELETE", "/", "", map[string]string{"namespace": "dotvirt", "name": "drafts"})
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	pvcs := a.CS.CoreV1().PersistentVolumeClaims
	if _, err := pvcs("dotvirt").Get(context.Background(), "drafts", metav1.GetOptions{}); err == nil {
		t.Fatal("foreign volume survived its delete")
	}
	kept, err := pvcs("stornas-system").Get(context.Background(), "drafts", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("same-named system volume was deleted: %v", err)
	}
	if q := kept.Spec.Resources.Requests["storage"]; q.String() != "1Gi" {
		t.Fatalf("same-named system volume was resized to %s", q.String())
	}
}

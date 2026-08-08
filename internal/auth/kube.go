package auth

import (
	"context"
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var localUserGVR = schema.GroupVersionResource{
	Group: "storage.stornas.io", Version: "v1alpha1", Resource: "localusers",
}

// annotGenerated marks a LocalUser still on its bootstrap-generated
// password; cleared on the first self-service change.
const annotGenerated = "stornas.io/generated-password"

// KubeSource resolves users from LocalUser CRs and their password Secrets
// in the appliance namespace. Login is rare, so every lookup is a live
// read; no cache to invalidate.
type KubeSource struct {
	Dyn       dynamic.Interface
	CS        kubernetes.Interface
	Namespace string
}

func (s *KubeSource) Lookup(ctx context.Context, username string) (User, string, error) {
	u, err := s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).Get(ctx, username, metav1.GetOptions{})
	if err != nil {
		return User{}, "", err
	}
	role, _, _ := unstructured.NestedString(u.Object, "spec", "role")
	ref, _, _ := unstructured.NestedString(u.Object, "spec", "passwordSecretRef")
	if role == "" || ref == "" {
		return User{}, "", fmt.Errorf("localuser %s has no role or secret ref", username)
	}
	secret, err := s.CS.CoreV1().Secrets(s.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return User{}, "", err
	}
	return User{Name: username, Role: role}, string(secret.Data["password"]), nil
}

// Bootstrap creates the admin user and its password Secret when no
// LocalUser exists yet. Returns the generated password exactly once (first
// boot); afterwards it lives only in the Secret.
func (s *KubeSource) Bootstrap(ctx context.Context) (string, error) {
	users, err := s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	if len(users.Items) > 0 {
		return "", nil
	}

	password := rand.Text()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "admin-password", Namespace: s.Namespace},
		Data:       map[string][]byte{"password": []byte(password)},
	}
	if _, err := s.CS.CoreV1().Secrets(s.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	admin := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.stornas.io/v1alpha1",
		"kind":       "LocalUser",
		"metadata": map[string]any{
			"name":        "admin",
			"namespace":   s.Namespace,
			"annotations": map[string]any{annotGenerated: "true"},
		},
		"spec": map[string]any{
			"role":              "admin",
			"passwordSecretRef": "admin-password",
		},
	}}
	if _, err := s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).Create(ctx, admin, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", err
	}
	return password, nil
}

// MustChange reports whether the user still runs on a generated password.
func (s *KubeSource) MustChange(ctx context.Context, username string) bool {
	u, err := s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).Get(ctx, username, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return u.GetAnnotations()[annotGenerated] == "true"
}

// UpdatePassword rotates the referenced Secret in place (the agent's smb
// reconciler watches it) and clears the generated-password mark.
func (s *KubeSource) UpdatePassword(ctx context.Context, username, password string) error {
	u, err := s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).Get(ctx, username, metav1.GetOptions{})
	if err != nil {
		return err
	}
	ref, _, _ := unstructured.NestedString(u.Object, "spec", "passwordSecretRef")
	if ref == "" {
		return fmt.Errorf("localuser %s has no secret ref", username)
	}
	secret, err := s.CS.CoreV1().Secrets(s.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data["password"] = []byte(password)
	if _, err := s.CS.CoreV1().Secrets(s.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		return err
	}
	if u.GetAnnotations()[annotGenerated] != "" {
		patch := []byte(`{"metadata":{"annotations":{"` + annotGenerated + `":null}}}`)
		_, err = s.Dyn.Resource(localUserGVR).Namespace(s.Namespace).Patch(
			ctx, username, types.MergePatchType, patch, metav1.PatchOptions{})
	}
	return err
}

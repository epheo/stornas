// Package console carries what a host's login screen and MOTD tell the
// admin: where the UI is, which certificate it serves, and the first-boot
// password while it is still the generated one. The server publishes the
// fingerprint; the agent renders the files on every node.
package console

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMapName holds no secret: the fingerprint is public by nature.
	ConfigMapName  = "stornas-console"
	KeyFingerprint = "tlsFingerprint"
	// UIPort is the Service NodePort in image/manifests/stornas/server.yaml.
	UIPort = 30080
)

// Publish stores the serving certificate's fingerprint for the agents.
func Publish(ctx context.Context, cs kubernetes.Interface, ns, fingerprint string) error {
	cms := cs.CoreV1().ConfigMaps(ns)
	cm, err := cms.Get(ctx, ConfigMapName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: ns},
			Data:       map[string]string{KeyFingerprint: fingerprint},
		}, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	case cm.Data[KeyFingerprint] == fingerprint:
		return nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[KeyFingerprint] = fingerprint
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// Render returns the login screen and MOTD text. host is the node's
// address; passwords maps a user to the generated password it still runs
// on. agetty expands backslash escapes, and none of these values holds one.
func Render(host, fingerprint string, passwords map[string]string) string {
	if host == "" {
		host = "<this host>"
	}
	lines := []string{"stornas web UI: https://" + net.JoinHostPort(host, strconv.Itoa(UIPort))}
	if fingerprint != "" {
		lines = append(lines, "TLS certificate SHA-256: "+fingerprint)
	}
	users := make([]string, 0, len(passwords))
	for u := range passwords {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		lines = append(lines, fmt.Sprintf("Initial password for %s: %s (the UI asks to change it)", u, passwords[u]))
	}
	return strings.Join(lines, "\n") + "\n\n"
}

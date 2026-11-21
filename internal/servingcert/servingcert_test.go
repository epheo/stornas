package servingcert

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// The first call stores a certificate; a restart reuses it, so the
// fingerprint an admin checked once stays valid.
func TestEnsureCreatesThenReuses(t *testing.T) {
	cs := k8sfake.NewClientset()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a, err := Ensure(context.Background(), cs, "ns", "tls", []string{"stornas", "192.168.1.10"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Leaf.DNSNames; len(got) != 1 || got[0] != "stornas" {
		t.Fatalf("DNS SANs = %v", got)
	}
	if got := a.Leaf.IPAddresses; len(got) != 1 || got[0].String() != "192.168.1.10" {
		t.Fatalf("IP SANs = %v", got)
	}
	s, err := cs.CoreV1().Secrets("ns").Get(context.Background(), "tls", metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeTLS {
		t.Fatalf("secret %v err %v", s, err)
	}
	b, err := Ensure(context.Background(), cs, "ns", "tls", nil, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Leaf.Raw) != Fingerprint(b.Leaf.Raw) {
		t.Fatal("restart replaced a valid certificate")
	}
}

// Close to expiry the certificate is replaced in place.
func TestEnsureRenewsBeforeExpiry(t *testing.T) {
	cs := k8sfake.NewClientset()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a, err := Ensure(context.Background(), cs, "ns", "tls", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Ensure(context.Background(), cs, "ns", "tls", nil, a.Leaf.NotAfter.Add(-renewLead/2))
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(a.Leaf.Raw) == Fingerprint(b.Leaf.Raw) {
		t.Fatal("certificate near expiry was kept")
	}
	if b.Leaf.NotAfter.Sub(b.Leaf.NotBefore) > validity {
		t.Fatalf("validity %v above the Apple limit", b.Leaf.NotAfter.Sub(b.Leaf.NotBefore))
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := Fingerprint([]byte("x"))
	if len(strings.Split(fp, ":")) != sha256.Size || strings.ToUpper(fp) != fp {
		t.Fatalf("fingerprint %q", fp)
	}
}

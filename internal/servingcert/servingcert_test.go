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
	a, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf("stornas", "192.168.1.10"), now)
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
	b, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf(), now.Add(24*time.Hour))
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
	a, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf(), now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf(), a.Leaf.NotAfter.Add(-renewLead/2))
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

// A node that joins later keeps the certificate: the fingerprint an admin
// checked must not change on the next restart.
func TestEnsureKeepsCertificateWhenNodesJoin(t *testing.T) {
	cs := k8sfake.NewClientset()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf("stornas", "node1"), now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Ensure(context.Background(), cs, "ns", "tls", hostsOf("stornas", "node1", "node2"), now)
	if err != nil || Fingerprint(a.Leaf.Raw) != Fingerprint(b.Leaf.Raw) {
		t.Fatalf("a joining node replaced the certificate: %v", err)
	}
}

// The rotator swaps the served certificate and republishes on renewal.
func TestRotatorRenews(t *testing.T) {
	var published []string
	r := &Rotator{
		CS: k8sfake.NewClientset(), Namespace: "ns", Name: "tls",
		Hosts: func(context.Context) []string { return []string{"stornas"} },
		Published: func(_ context.Context, fp string) error {
			published = append(published, fp)
			return nil
		},
	}
	ctx := context.Background()
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := r.GetCertificate(nil)
	if err := r.ensure(ctx, time.Now()); err != nil || len(published) != 1 {
		t.Fatalf("an unchanged certificate republished: %v %v", published, err)
	}
	if err := r.ensure(ctx, first.Leaf.NotAfter.Add(-renewLead/2)); err != nil {
		t.Fatal(err)
	}
	second, _ := r.GetCertificate(nil)
	if Fingerprint(first.Leaf.Raw) == Fingerprint(second.Leaf.Raw) || len(published) != 2 ||
		published[1] != Fingerprint(second.Leaf.Raw) {
		t.Fatalf("renewal not served or published: %v", published)
	}
}

// The daily wake reuses the stored certificate without listing Nodes.
func TestEnsureListsHostsOnlyToGenerate(t *testing.T) {
	cs := k8sfake.NewClientset()
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	calls := 0
	hosts := func(context.Context) []string { calls++; return nil }
	for range 3 {
		if _, err := Ensure(context.Background(), cs, "ns", "tls", hosts, now); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("hosts listed %d times, want once", calls)
	}
}

func hostsOf(hosts ...string) func(context.Context) []string {
	return func(context.Context) []string { return hosts }
}

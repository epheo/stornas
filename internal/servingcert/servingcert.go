// Package servingcert gives the UI one TLS certificate across restarts:
// self-signed on first start, then read back from a Secret.
package servingcert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Apple platforms refuse TLS server certificates valid longer than 825 days.
const (
	validity  = 825 * 24 * time.Hour
	renewLead = 30 * 24 * time.Hour
)

// Ensure returns the certificate stored in the Secret, replacing it when
// absent, unreadable, or within renewLead of expiry. hosts become the SANs
// of a new certificate only: a node joining later must not change the
// fingerprint the admin already checked.
func Ensure(ctx context.Context, cs kubernetes.Interface, ns, name string, hosts []string, now time.Time) (tls.Certificate, error) {
	secrets := cs.CoreV1().Secrets(ns)
	s, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		s = nil
	case err != nil:
		return tls.Certificate{}, err
	}
	if s != nil {
		if c, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]); err == nil &&
			c.Leaf != nil && now.Add(renewLead).Before(c.Leaf.NotAfter) {
			return c, nil
		}
	}
	certPEM, keyPEM, err := generate(hosts, now)
	if err != nil {
		return tls.Certificate{}, err
	}
	data := map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
	if s == nil {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}, metav1.CreateOptions{})
	} else {
		s.Data = data
		_, err = secrets.Update(ctx, s, metav1.UpdateOptions{})
	}
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// Rotator serves the current certificate and replaces it before expiry:
// a server that never restarts must not end up serving an expired one.
type Rotator struct {
	CS        kubernetes.Interface
	Namespace string
	Name      string
	Hosts     func(context.Context) []string
	// Published runs on every new certificate with its fingerprint.
	Published func(context.Context, string) error

	cur atomic.Pointer[tls.Certificate]
}

// Load ensures and publishes the certificate once, before serving.
func (r *Rotator) Load(ctx context.Context) error {
	return r.ensure(ctx, time.Now())
}

func (r *Rotator) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// Run checks the wall clock at least daily: a timer runs on the monotonic
// clock, which stops across suspend and ignores a stepped wall clock. A
// failed renewal retries at the next wake; nothing else would wake it
// before the certificate expires.
func (r *Rotator) Run(ctx context.Context) {
	for {
		wait := min(time.Until(r.cur.Load().Leaf.NotAfter.Add(-renewLead)), 24*time.Hour)
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(wait, time.Hour)):
		}
		if err := r.ensure(ctx, time.Now()); err != nil {
			log.Printf("serving certificate renewal: %v", err)
		}
	}
}

func (r *Rotator) ensure(ctx context.Context, now time.Time) error {
	c, err := Ensure(ctx, r.CS, r.Namespace, r.Name, r.Hosts(ctx), now)
	if err != nil {
		return err
	}
	if old := r.cur.Load(); old != nil && Fingerprint(old.Leaf.Raw) == Fingerprint(c.Leaf.Raw) {
		return nil
	}
	if err := r.Published(ctx, Fingerprint(c.Leaf.Raw)); err != nil {
		return err
	}
	r.cur.Store(&c)
	return nil
}

// Fingerprint is the SHA-256 of the DER certificate, colon-separated
// uppercase hex as openssl and browsers print it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func generate(hosts []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "stornas"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(-time.Hour).Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

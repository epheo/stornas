package console

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestRender(t *testing.T) {
	text := Render("192.168.1.10", "AB:CD", map[string]string{"admin": "PW"})
	for _, want := range []string{"https://192.168.1.10:30080", "SHA-256: AB:CD", "admin: PW"} {
		if !strings.Contains(text, want) {
			t.Fatalf("banner lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `\`) {
		t.Fatalf("agetty would expand an escape:\n%s", text)
	}
	if text := Render("fd00::1", "", nil); !strings.Contains(text, "https://[fd00::1]:30080") {
		t.Fatalf("IPv6 address not bracketed:\n%s", text)
	}
	// A changed password leaves the console.
	if text := Render("", "AB:CD", nil); strings.Contains(text, "password") || !strings.Contains(text, "<this host>") {
		t.Fatalf("banner:\n%s", text)
	}
}

func TestPublishUpdatesInPlace(t *testing.T) {
	cs := k8sfake.NewClientset()
	ctx := context.Background()
	for _, fp := range []string{"AA", "AA", "BB"} {
		if err := Publish(ctx, cs, "ns", fp); err != nil {
			t.Fatal(err)
		}
	}
	cm, err := cs.CoreV1().ConfigMaps("ns").Get(ctx, ConfigMapName, metav1.GetOptions{})
	if err != nil || cm.Data[KeyFingerprint] != "BB" {
		t.Fatalf("cm %v err %v", cm, err)
	}
}

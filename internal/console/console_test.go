package console

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestRender(t *testing.T) {
	issue, motd := Render("AB:CD", map[string]string{"admin": "PW"})
	for _, want := range []string{`https://\4:30080`, "SHA-256: AB:CD", "admin: PW"} {
		if !strings.Contains(issue, want) {
			t.Fatalf("issue lacks %q:\n%s", want, issue)
		}
	}
	if strings.Contains(motd, `\4`) || !strings.Contains(motd, "admin: PW") {
		t.Fatalf("motd:\n%s", motd)
	}
	// A changed password leaves the console.
	issue, _ = Render("AB:CD", nil)
	if strings.Contains(issue, "password") {
		t.Fatalf("issue still shows a password:\n%s", issue)
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

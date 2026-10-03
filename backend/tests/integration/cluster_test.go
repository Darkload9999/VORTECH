//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange/kube"
)

// realCluster connects to TEST_KUBECONFIG (make cluster-up creates a local
// k3d cluster) or skips.
func realCluster(t *testing.T) *kube.KubeCluster {
	t.Helper()
	path := os.Getenv("TEST_KUBECONFIG")
	if path == "" {
		t.Skip("TEST_KUBECONFIG not set; skipping real-cluster tests")
	}
	k, err := kube.NewKubeCluster(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Ping(context.Background()); err != nil {
		t.Fatalf("cluster unreachable: %v", err)
	}
	return k
}

func templateSpec(t *testing.T, slug string) cyberrange.Spec {
	t.Helper()
	for _, rt := range loadNexora(t, nexoraDir).RangeTemplates {
		if rt.Slug == slug {
			return rt.Spec()
		}
	}
	t.Fatalf("template %s not found", slug)
	return cyberrange.Spec{}
}

// provisionReal applies a range from a template and waits for it.
func provisionReal(t *testing.T, k *kube.KubeCluster, slug string) string {
	t.Helper()
	id := uuid.New()
	ns := cyberrange.NamespaceFor(id)
	spec := templateSpec(t, slug)
	secrets := map[string]string{}
	for _, key := range spec.SecretKeys() {
		secrets[key] = "it-" + uuid.NewString()
	}
	ctx := context.Background()
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := k.Delete(cctx, ns); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if err := k.WaitGone(cctx, ns); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if _, err := k.Apply(ctx, kube.Build(kube.BuildInput{
		RangeID: id, PlayerID: uuid.New(), Namespace: ns, ExpiresAt: time.Now().Add(time.Hour), Spec: spec, Secrets: secrets,
	})); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Minute) // first run pulls images
	defer cancel()
	if err := k.WaitReady(wctx, ns); err != nil {
		t.Fatalf("%s not ready: %v", slug, err)
	}
	return ns
}

// TestRangeOnRealCluster provisions NEXORA ranges in K3s and proves the
// isolation the manifests promise: hardened pods start under Pod Security
// "restricted", only declared flows are reachable, and there is no route
// to the internet, the Kubernetes API or another player's range.
func TestRangeOnRealCluster(t *testing.T) {
	k := realCluster(t)
	ctx := context.Background()

	corp := provisionReal(t, k, "nexora-corp-net")
	other := provisionReal(t, k, "nexora-web-basics")

	run := func(ns, workload string, cmd ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, errOut, err := k.ExecOutput(cctx, ns, workload, cmd...)
		return out + errOut, err
	}
	fetch := func(ns, from, url string) (string, error) {
		return run(ns, from, "wget", "-q", "-O-", "-T", "4", url)
	}

	// Declared flows work.
	if out, err := fetch(corp, "workstation", "http://web01:8080/"); err != nil || !strings.Contains(out, "nginx") {
		t.Errorf("workstation → web01: %v %q", err, out)
	}
	if out, err := fetch(corp, "workstation", "http://app01:8080/"); err != nil || !strings.Contains(out, "Hostname") {
		t.Errorf("workstation → app01: %v %q", err, out)
	}
	if out, err := fetch(corp, "web01", "http://app01:8080/"); err != nil || !strings.Contains(out, "Hostname") {
		t.Errorf("web01 → app01: %v %q", err, out)
	}

	// Undeclared flows are blocked by default-deny. K3s's policy engine
	// rejects them (ICMP port unreachable, seen as "Connection refused");
	// other engines drop them (timeout). db01 really listens on 5432, so a
	// refusal there can only come from the policy, not a closed port.
	if out, err := run(corp, "db01", "pg_isready", "-h", "127.0.0.1", "-p", "5432"); err != nil {
		t.Fatalf("control: db01 must be listening: %v %q", err, out)
	}
	blocked := []struct{ name, ns, from, url string }{
		{"workstation → db01 (segmented)", corp, "workstation", "http://db01:5432/"},
		{"web01 → db01 (segmented)", corp, "web01", "http://db01:5432/"},
		{"internet", corp, "workstation", "http://1.1.1.1/"},
		{"kubernetes API", corp, "workstation", "https://kubernetes.default.svc/"},
		{"another player's range", corp, "workstation", "http://web01." + other + ".svc.cluster.local:8080/"},
		{"from another range", other, "workstation", "http://web01." + corp + ".svc.cluster.local:8080/"},
	}
	for _, b := range blocked {
		if out, err := fetch(b.ns, b.from, b.url); err == nil || !(strings.Contains(out, "refused") || strings.Contains(out, "timed out")) {
			t.Errorf("%s must be dropped by policy, got err=%v %q", b.name, err, out)
		}
	}

	// Workloads run unprivileged on a read-only root filesystem.
	if out, err := run(corp, "workstation", "id", "-u"); err != nil || strings.TrimSpace(out) != "1000" {
		t.Errorf("workstation uid: %v %q", err, out)
	}
	if out, err := run(corp, "workstation", "touch", "/etc/pwned"); err == nil {
		t.Errorf("root filesystem is writable: %q", out)
	}
	if _, err := run(corp, "workstation", "touch", "/home/analyst/notes"); err != nil {
		t.Errorf("home must be writable: %v", err)
	}
	// The database got its generated password from the Secret.
	if out, err := run(corp, "db01", "sh", "-c", `test -n "$POSTGRES_PASSWORD" && echo set`); err != nil || strings.TrimSpace(out) != "set" {
		t.Errorf("db01 secret env: %v %q", err, out)
	}

	managed, err := k.ListManaged(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range managed {
		if m.Name == corp || m.Name == other {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("managed namespaces %+v", managed)
	}
}

// TestWorkerIdentityIsConfined runs as the worker's service account
// (TEST_WORKER_KUBECONFIG, see make cluster-worker-kubeconfig) and proves
// RBAC plus the admission policy keep it inside range namespaces.
func TestWorkerIdentityIsConfined(t *testing.T) {
	path := os.Getenv("TEST_WORKER_KUBECONFIG")
	if path == "" {
		t.Skip("TEST_WORKER_KUBECONFIG not set; skipping worker confinement tests")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	managed := map[string]string{kube.LabelManagedBy: kube.ManagedByValue, "pod-security.kubernetes.io/enforce": "restricted"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vortech-it-escape"}, StringData: map[string]string{"k": "v"}}

	denied := []struct {
		name string
		fn   func() error
	}{
		{"secret in default", func() error {
			_, err := cs.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})
			return err
		}},
		{"secret in kube-system", func() error {
			_, err := cs.CoreV1().Secrets("kube-system").Create(ctx, secret, metav1.CreateOptions{})
			return err
		}},
		{"read kube-system secrets", func() error {
			_, err := cs.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
			return err
		}},
		{"unlabelled namespace", func() error {
			_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "range-" + uuid.NewString()}}, metav1.CreateOptions{})
			return err
		}},
		{"non-range namespace name", func() error {
			_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "evil-" + uuid.NewString()[:8], Labels: managed}}, metav1.CreateOptions{})
			return err
		}},
		{"privileged range namespace", func() error {
			labels := map[string]string{kube.LabelManagedBy: kube.ManagedByValue, "pod-security.kubernetes.io/enforce": "privileged"}
			_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "range-" + uuid.NewString(), Labels: labels}}, metav1.CreateOptions{})
			return err
		}},
		{"delete kube-system", func() error {
			return cs.CoreV1().Namespaces().Delete(ctx, "kube-system", metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}})
		}},
		{"relabel default as managed", func() error {
			patch := []byte(`{"metadata":{"labels":{"vortech.io/managed-by":"vortech"}}}`)
			_, err := cs.CoreV1().Namespaces().Patch(ctx, "default", types.MergePatchType, patch, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
			return err
		}},
		{"deployment in default", func() error {
			d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "escape"}, Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "docker.io/library/alpine:3.20"}}}},
			}}
			_, err := cs.AppsV1().Deployments("default").Create(ctx, d, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
			return err
		}},
	}
	for _, d := range denied {
		err := d.fn()
		if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
			t.Errorf("%s: expected denial, got %v", d.name, err)
		}
	}

	// A properly labelled range namespace is allowed (dry run).
	ok := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "range-" + uuid.NewString(), Labels: managed}}
	if _, err := cs.CoreV1().Namespaces().Create(ctx, ok, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatalf("managed namespace must be allowed: %v", err)
	}
}

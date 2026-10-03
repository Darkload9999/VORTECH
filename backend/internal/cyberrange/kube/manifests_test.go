package kube

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	corev1 "k8s.io/api/core/v1"
)

func buildTest() Bundle {
	id := uuid.New()
	return Build(BuildInput{
		RangeID: id, PlayerID: uuid.New(), Namespace: cyberrange.NamespaceFor(id),
		ExpiresAt: time.Now().Add(time.Hour), Spec: testSpec(),
		Secrets: map[string]string{"web_admin": "s3cret"},
	})
}

func TestNamespaceIsolationLabels(t *testing.T) {
	b := buildTest()
	l := b.Namespace.Labels
	if l["pod-security.kubernetes.io/enforce"] != "restricted" || l[LabelManagedBy] != ManagedByValue || l[LabelRangeID] == "" || l[LabelPlayerID] == "" {
		t.Fatalf("namespace labels %v", l)
	}
	if !strings.HasPrefix(b.Namespace.Name, "range-") || b.Namespace.Annotations[AnnotationExpiry] == "" {
		t.Fatalf("namespace %s / %v", b.Namespace.Name, b.Namespace.Annotations)
	}
}

func TestPodsAreHardened(t *testing.T) {
	b := buildTest()
	if len(b.Deployments) != 2 || len(b.Services) != 1 {
		t.Fatalf("deployments=%d services=%d", len(b.Deployments), len(b.Services))
	}
	for _, d := range b.Deployments {
		pod := d.Spec.Template.Spec
		if pod.HostNetwork || pod.HostPID || pod.HostIPC {
			t.Errorf("%s uses host namespaces", d.Name)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("%s mounts a service account token", d.Name)
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Errorf("%s enables service links", d.Name)
		}
		if psc := pod.SecurityContext; psc == nil || !*psc.RunAsNonRoot || *psc.RunAsUser == 0 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s pod security context %+v", d.Name, psc)
		}
		for _, v := range pod.Volumes {
			if v.HostPath != nil || v.EmptyDir == nil || v.EmptyDir.SizeLimit == nil {
				t.Errorf("%s volume %s must be a size-limited emptyDir", d.Name, v.Name)
			}
		}
		for _, c := range pod.Containers {
			sc := c.SecurityContext
			if sc == nil || *sc.Privileged || *sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 {
				t.Errorf("%s container security context %+v", d.Name, sc)
			}
			if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() || !c.Resources.Requests.Cpu().Equal(*c.Resources.Limits.Cpu()) {
				t.Errorf("%s must have equal requests and limits", d.Name)
			}
		}
	}
	ws := b.Deployments[0].Spec.Template.Spec.Containers[0]
	if !*ws.SecurityContext.ReadOnlyRootFilesystem || len(ws.VolumeMounts) != 1 || ws.VolumeMounts[0].MountPath != "/tmp" {
		t.Fatalf("workstation filesystem %+v %+v", ws.SecurityContext, ws.VolumeMounts)
	}
}

func TestSecretsStayInKubernetes(t *testing.T) {
	b := buildTest()
	if b.Secret == nil || b.Secret.StringData["web_admin"] != "s3cret" {
		t.Fatal("secret not rendered")
	}
	web := b.Deployments[1].Spec.Template.Spec.Containers[0]
	for _, e := range web.Env {
		if e.Name == "ADMIN_PASSWORD" && (e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Key != "web_admin") {
			t.Fatalf("secret env must reference the Secret, got %+v", e)
		}
	}
}

func TestQuotaForbidsEscapes(t *testing.T) {
	q := buildTest().ResourceQuota.Spec.Hard
	for _, r := range []corev1.ResourceName{corev1.ResourceServicesNodePorts, corev1.ResourceServicesLoadBalancers, corev1.ResourcePersistentVolumeClaims} {
		if v, ok := q[r]; !ok || !v.IsZero() {
			t.Errorf("quota must set %s to 0", r)
		}
	}
	cpu, mem := q[corev1.ResourceLimitsCPU], q[corev1.ResourceLimitsMemory]
	if cpu.MilliValue() != 600 || mem.Value() != 320*1024*1024 {
		t.Fatalf("quota %v", q)
	}
}

func TestNetworkPolicies(t *testing.T) {
	np := buildTest().NetworkPolicies
	if len(np) != 4 {
		t.Fatalf("expected default-deny, dns and one rule pair, got %d", len(np))
	}
	deny := np[0]
	if deny.Name != "default-deny" || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PolicyTypes) != 2 || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Fatalf("default deny %+v", deny.Spec)
	}
	dns := np[1].Spec.Egress[0]
	if dns.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" || dns.Ports[0].Port.IntValue() != 53 {
		t.Fatalf("dns egress %+v", dns)
	}
	in, out := np[2], np[3]
	if in.Spec.PodSelector.MatchLabels[LabelWorkload] != "web01" || in.Spec.Ingress[0].From[0].PodSelector.MatchLabels[LabelWorkload] != "workstation" ||
		in.Spec.Ingress[0].Ports[0].Port.IntValue() != 8080 || in.Spec.Ingress[0].From[0].NamespaceSelector != nil {
		t.Fatalf("ingress rule %+v", in.Spec)
	}
	if out.Spec.PodSelector.MatchLabels[LabelWorkload] != "workstation" || out.Spec.Egress[0].To[0].PodSelector.MatchLabels[LabelWorkload] != "web01" {
		t.Fatalf("egress rule %+v", out.Spec)
	}
	for _, p := range np {
		for _, e := range p.Spec.Egress {
			for _, to := range e.To {
				if to.IPBlock != nil {
					t.Errorf("%s allows an IP block (internet/platform egress)", p.Name)
				}
			}
		}
	}
}

func testSpec() cyberrange.Spec {
	return cyberrange.Spec{
		Workloads: []cyberrange.Workload{
			{Name: "workstation", Role: "workstation", Image: "docker.io/library/alpine:3.20", CPUMillis: 500, MemoryMiB: 256, StorageMiB: 64,
				RunAsUser: 1000, ReadOnlyRoot: true, WritablePaths: []string{"/tmp"}, Terminal: true},
			{Name: "web01", Role: "web", Image: "docker.io/nginxinc/nginx-unprivileged:1.27-alpine", CPUMillis: 100, MemoryMiB: 64, StorageMiB: 32,
				RunAsUser: 101, Ports: []cyberrange.Port{{Name: "http", Port: 8080}}, WritablePaths: []string{"/tmp"},
				Env: map[string]string{"ADMIN_PASSWORD": "secret:web_admin"}},
		},
		Network: []cyberrange.NetworkRule{{From: "workstation", To: "web01", Port: 8080}},
	}
}

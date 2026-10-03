package kube

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Darkload9999/VORTECH/backend/internal/cyberrange"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Labels and annotations placed on every range object.
const (
	LabelManagedBy   = "vortech.io/managed-by"
	ManagedByValue   = "vortech"
	LabelRangeID     = "vortech.io/range-id"
	LabelPlayerID    = "vortech.io/player-id"
	LabelWorkload    = "vortech.io/workload"
	LabelQuarantined = "vortech.io/quarantined"
	AnnotationExpiry = "vortech.io/expires-at"
	SecretName       = "range-secrets"
)

// Bundle is the full set of objects that make up one range.
type Bundle struct {
	Namespace       *corev1.Namespace
	ResourceQuota   *corev1.ResourceQuota
	LimitRange      *corev1.LimitRange
	NetworkPolicies []*networkingv1.NetworkPolicy
	Secret          *corev1.Secret
	ServiceAccount  *corev1.ServiceAccount
	Deployments     []*appsv1.Deployment
	Services        []*corev1.Service
}

// BuildInput identifies the range being built.
type BuildInput struct {
	RangeID   uuid.UUID
	PlayerID  uuid.UUID
	Namespace string
	ExpiresAt time.Time
	Spec      cyberrange.Spec
	// Secrets maps generated secret keys to values (see Spec.SecretKeys).
	Secrets map[string]string
}

// Build renders the Kubernetes objects for a range. It is pure, so the
// security posture of every object is unit-tested without a cluster.
//
// Isolation: Pod Security "restricted" is enforced on the namespace;
// pods run as non-root with no privilege escalation, all capabilities
// dropped, RuntimeDefault seccomp, a read-only root filesystem (writable
// paths are size-limited emptyDirs) and no service-account token;
// default-deny NetworkPolicies allow only DNS and the template's
// workload-to-workload rules (no internet, platform or other ranges); a
// ResourceQuota caps resources and forbids volumes claims, NodePorts and
// LoadBalancers.
func Build(in BuildInput) Bundle {
	ns := in.Namespace
	base := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelRangeID:   in.RangeID.String(),
	}
	cpu, mem, storage := in.Spec.Totals()

	b := Bundle{
		Namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: merge(base, map[string]string{
				LabelPlayerID:                                in.PlayerID.String(),
				"pod-security.kubernetes.io/enforce":         "restricted",
				"pod-security.kubernetes.io/enforce-version": "latest",
				"pod-security.kubernetes.io/audit":           "restricted",
				"pod-security.kubernetes.io/warn":            "restricted",
			}),
			Annotations: map[string]string{AnnotationExpiry: in.ExpiresAt.UTC().Format(time.RFC3339)},
		}},
		ResourceQuota: &corev1.ResourceQuota{
			ObjectMeta: meta("range-quota", ns, base),
			Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:              milli(cpu),
				corev1.ResourceLimitsCPU:                milli(cpu),
				corev1.ResourceRequestsMemory:           mib(mem),
				corev1.ResourceLimitsMemory:             mib(mem),
				corev1.ResourceRequestsEphemeralStorage: mib(storage + int32(len(in.Spec.Workloads))*16),
				corev1.ResourceLimitsEphemeralStorage:   mib(storage + int32(len(in.Spec.Workloads))*16),
				corev1.ResourcePods:                     count(len(in.Spec.Workloads) * 2),
				corev1.ResourceServices:                 count(len(in.Spec.Workloads)),
				corev1.ResourceServicesNodePorts:        count(0),
				corev1.ResourceServicesLoadBalancers:    count(0),
				corev1.ResourcePersistentVolumeClaims:   count(0),
				corev1.ResourceSecrets:                  count(4),
				corev1.ResourceConfigMaps:               count(4),
			}},
		},
		LimitRange: &corev1.LimitRange{
			ObjectMeta: meta("range-limits", ns, base),
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				Max:  corev1.ResourceList{corev1.ResourceCPU: milli(4000), corev1.ResourceMemory: mib(8192)},
				Default: corev1.ResourceList{
					corev1.ResourceCPU: milli(100), corev1.ResourceMemory: mib(64), corev1.ResourceEphemeralStorage: mib(16),
				},
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU: milli(100), corev1.ResourceMemory: mib(64), corev1.ResourceEphemeralStorage: mib(16),
				},
			}}},
		},
		ServiceAccount: &corev1.ServiceAccount{
			ObjectMeta:                   meta("range-workload", ns, base),
			AutomountServiceAccountToken: ptr(false),
		},
		NetworkPolicies: networkPolicies(ns, base, in.Spec),
	}

	if keys := in.Spec.SecretKeys(); len(keys) > 0 {
		data := map[string]string{}
		for _, k := range keys {
			data[k] = in.Secrets[k]
		}
		b.Secret = &corev1.Secret{ObjectMeta: meta(SecretName, ns, base), Type: corev1.SecretTypeOpaque, StringData: data}
	}

	for _, w := range in.Spec.Workloads {
		b.Deployments = append(b.Deployments, deployment(ns, base, w))
		if len(w.Ports) > 0 {
			b.Services = append(b.Services, service(ns, base, w))
		}
	}
	return b
}

func deployment(ns string, base map[string]string, w cyberrange.Workload) *appsv1.Deployment {
	labels := merge(base, map[string]string{LabelWorkload: w.Name})
	selector := map[string]string{LabelWorkload: w.Name}

	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for i, p := range w.WritablePaths {
		name := fmt.Sprintf("writable-%d", i)
		// Each writable path gets the workload's storage budget as a hard
		// cap; the quota bounds the total.
		volumes = append(volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(mib(w.StorageMiB))},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: p})
	}

	var env []corev1.EnvVar
	keys := make([]string, 0, len(w.Env))
	for k := range w.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := w.Env[k]
		if sk, ok := strings.CutPrefix(v, cyberrange.SecretPrefix); ok {
			env = append(env, corev1.EnvVar{Name: k, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: SecretName}, Key: sk,
			}}})
			continue
		}
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	var ports []corev1.ContainerPort
	for _, p := range w.Ports {
		ports = append(ports, corev1.ContainerPort{Name: p.Name, ContainerPort: p.Port, Protocol: corev1.ProtocolTCP})
	}

	res := corev1.ResourceList{
		corev1.ResourceCPU:              milli(w.CPUMillis),
		corev1.ResourceMemory:           mib(w.MemoryMiB),
		corev1.ResourceEphemeralStorage: mib(w.StorageMiB + 16),
	}
	uid := w.RunAsUser

	return &appsv1.Deployment{
		ObjectMeta: meta(w.Name, ns, labels),
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Hostname:                      w.Name,
					ServiceAccountName:            "range-workload",
					AutomountServiceAccountToken:  ptr(false),
					EnableServiceLinks:            ptr(false),
					TerminationGracePeriodSeconds: ptr(int64(5)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      &uid,
						RunAsGroup:     &uid,
						FSGroup:        &uid,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            w.Name,
						Image:           w.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         w.Command,
						Args:            w.Args,
						Ports:           ports,
						Env:             env,
						Resources:       corev1.ResourceRequirements{Requests: res, Limits: res},
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{
							Privileged:               ptr(false),
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(w.ReadOnlyRoot),
							RunAsNonRoot:             ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}

func service(ns string, base map[string]string, w cyberrange.Workload) *corev1.Service {
	var ports []corev1.ServicePort
	for _, p := range w.Ports {
		ports = append(ports, corev1.ServicePort{Name: p.Name, Port: p.Port, TargetPort: intstr.FromInt32(p.Port), Protocol: corev1.ProtocolTCP})
	}
	return &corev1.Service{
		ObjectMeta: meta(w.Name, ns, merge(base, map[string]string{LabelWorkload: w.Name})),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{LabelWorkload: w.Name},
			Ports:    ports,
		},
	}
}

func networkPolicies(ns string, base map[string]string, s cyberrange.Spec) []*networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	both := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}

	policies := []*networkingv1.NetworkPolicy{
		{ // Nothing in, nothing out unless allowed below.
			ObjectMeta: meta("default-deny", ns, base),
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: both},
		},
		{ // Name resolution via cluster DNS only.
			ObjectMeta: meta("allow-dns", ns, base),
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				}},
			},
		},
	}

	ports := map[string][]cyberrange.Port{}
	for _, w := range s.Workloads {
		ports[w.Name] = w.Ports
	}
	for i, r := range s.Network {
		var pp []networkingv1.NetworkPolicyPort
		for _, p := range ports[r.To] {
			if r.Port == 0 || r.Port == p.Port {
				port := intstr.FromInt32(p.Port)
				pp = append(pp, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
			}
		}
		from := map[string]string{LabelWorkload: r.From}
		to := map[string]string{LabelWorkload: r.To}
		policies = append(policies,
			&networkingv1.NetworkPolicy{
				ObjectMeta: meta(fmt.Sprintf("allow-%d-%s-to-%s-in", i, r.From, r.To), ns, base),
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: to},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Ingress: []networkingv1.NetworkPolicyIngressRule{{
						From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: from}}}, Ports: pp,
					}},
				},
			},
			&networkingv1.NetworkPolicy{
				ObjectMeta: meta(fmt.Sprintf("allow-%d-%s-to-%s-out", i, r.From, r.To), ns, base),
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: from},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
					Egress: []networkingv1.NetworkPolicyEgressRule{{
						To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: to}}}, Ports: pp,
					}},
				},
			},
		)
	}
	return policies
}

func meta(name, ns string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func milli(v int32) resource.Quantity {
	return *resource.NewMilliQuantity(int64(v), resource.DecimalSI)
}
func mib(v int32) resource.Quantity {
	return *resource.NewQuantity(int64(v)*1024*1024, resource.BinarySI)
}
func count(v int) resource.Quantity { return *resource.NewQuantity(int64(v), resource.DecimalSI) }
func ptr[T any](v T) *T             { return &v }

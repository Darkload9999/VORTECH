package kube

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// ResourceRef identifies one created Kubernetes object.
type ResourceRef struct {
	Kind string
	Name string
}

// ManagedNamespace is a range namespace found in the cluster.
type ManagedNamespace struct {
	Name        string
	RangeID     string
	Quarantined bool
	Terminating bool
}

// Cluster is the narrow set of cluster operations ranges need. KubeCluster
// implements it with client-go; the worker depends only on this interface.
type Cluster interface {
	Apply(ctx context.Context, b Bundle) ([]ResourceRef, error)
	WaitReady(ctx context.Context, namespace string) error
	Delete(ctx context.Context, namespace string) error
	WaitGone(ctx context.Context, namespace string) error
	Exists(ctx context.Context, namespace string) (bool, error)
	ListManaged(ctx context.Context) ([]ManagedNamespace, error)
	Quarantine(ctx context.Context, namespace string) error
}

// KubeCluster talks to the Kubernetes API with client-go.
type KubeCluster struct {
	cs   kubernetes.Interface
	rest *rest.Config // nil with a fake clientset (no exec)
	poll time.Duration
}

// NewKubeCluster connects using kubeconfigPath, or the in-cluster service
// account when the path is empty.
func NewKubeCluster(kubeconfigPath string) (*KubeCluster, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfigPath == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes config: %w", err)
	}
	cfg.UserAgent = "vortech-worker"
	cfg.QPS, cfg.Burst = 20, 40
	cfg.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	k := NewKubeClusterFromClient(cs)
	k.rest = cfg
	return k, nil
}

// NewKubeClusterFromClient wraps an existing clientset (tests use the fake).
func NewKubeClusterFromClient(cs kubernetes.Interface) *KubeCluster {
	return &KubeCluster{cs: cs, poll: 2 * time.Second}
}

// Ping verifies API server connectivity and the worker's permissions
// (readiness) with the cheapest call it actually needs.
func (k *KubeCluster) Ping(ctx context.Context) error {
	_, err := k.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue, Limit: 1})
	return err
}

// Apply creates every object of the bundle, tolerating objects that
// already exist so a retried job resumes where a crashed one stopped.
// NetworkPolicies are created before any workload: pods never run without
// their isolation in place.
func (k *KubeCluster) Apply(ctx context.Context, b Bundle) ([]ResourceRef, error) {
	ns := b.Namespace.Name
	var refs []ResourceRef
	step := func(kind, name string, err error) error {
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", kind, name, err)
		}
		refs = append(refs, ResourceRef{Kind: kind, Name: name})
		return nil
	}

	_, err := k.cs.CoreV1().Namespaces().Create(ctx, b.Namespace, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, gerr := k.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if gerr != nil {
			return nil, fmt.Errorf("get namespace %s: %w", ns, gerr)
		}
		if existing.Labels[LabelRangeID] != b.Namespace.Labels[LabelRangeID] {
			return nil, fmt.Errorf("namespace %s exists but belongs to another range", ns)
		}
		if existing.Status.Phase == corev1.NamespaceTerminating {
			return nil, fmt.Errorf("namespace %s is terminating", ns)
		}
		err = nil
	}
	if err := step("Namespace", ns, err); err != nil {
		return refs, err
	}
	if err := step("ServiceAccount", b.ServiceAccount.Name, ignoreObj(k.cs.CoreV1().ServiceAccounts(ns).Create(ctx, b.ServiceAccount, metav1.CreateOptions{}))); err != nil {
		return refs, err
	}
	if err := step("ResourceQuota", b.ResourceQuota.Name, ignoreObj(k.cs.CoreV1().ResourceQuotas(ns).Create(ctx, b.ResourceQuota, metav1.CreateOptions{}))); err != nil {
		return refs, err
	}
	if err := step("LimitRange", b.LimitRange.Name, ignoreObj(k.cs.CoreV1().LimitRanges(ns).Create(ctx, b.LimitRange, metav1.CreateOptions{}))); err != nil {
		return refs, err
	}
	for _, np := range b.NetworkPolicies {
		if err := step("NetworkPolicy", np.Name, ignoreObj(k.cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{}))); err != nil {
			return refs, err
		}
	}
	if b.Secret != nil {
		if err := step("Secret", b.Secret.Name, ignoreObj(k.cs.CoreV1().Secrets(ns).Create(ctx, b.Secret, metav1.CreateOptions{}))); err != nil {
			return refs, err
		}
	}
	for _, s := range b.Services {
		if err := step("Service", s.Name, ignoreObj(k.cs.CoreV1().Services(ns).Create(ctx, s, metav1.CreateOptions{}))); err != nil {
			return refs, err
		}
	}
	for _, d := range b.Deployments {
		if err := step("Deployment", d.Name, ignoreObj(k.cs.AppsV1().Deployments(ns).Create(ctx, d, metav1.CreateOptions{}))); err != nil {
			return refs, err
		}
	}
	return refs, nil
}

func ignoreObj[T any](_ T, err error) error { return err }

// ErrPermanent marks a provisioning failure that retrying cannot fix.
var ErrPermanent = errors.New("permanent provisioning failure")

// permanentReasons are container states that will not resolve by waiting.
var permanentReasons = []string{"InvalidImageName", "CreateContainerConfigError", "CreateContainerError"}

// WaitReady blocks until every Deployment in the namespace has a ready
// replica, the context ends, or a permanent failure is detected. On
// timeout the error carries a diagnostic summary of what is not ready.
func (k *KubeCluster) WaitReady(ctx context.Context, ns string) error {
	t := time.NewTicker(k.poll)
	defer t.Stop()
	for {
		ready, diag, err := k.readiness(ctx, ns)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("range not ready before deadline: %s", diag)
		case <-t.C:
		}
	}
}

func (k *KubeCluster) readiness(ctx context.Context, ns string) (bool, string, error) {
	deps, err := k.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "", fmt.Errorf("list deployments: %w", err)
	}
	if len(deps.Items) == 0 {
		return false, "no deployments", nil
	}
	var pending []string
	for _, d := range deps.Items {
		for _, c := range d.Status.Conditions {
			if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
				return false, "", fmt.Errorf("%w: deployment %s: %s", ErrPermanent, d.Name, c.Message)
			}
		}
		if d.Status.ReadyReplicas < 1 {
			pending = append(pending, d.Name)
		}
	}
	if len(pending) == 0 {
		return true, "", nil
	}
	sort.Strings(pending)

	pods, err := k.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, "", fmt.Errorf("list pods: %w", err)
	}
	var reasons []string
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				if slices.Contains(permanentReasons, w.Reason) {
					return false, "", fmt.Errorf("%w: %s/%s: %s: %s", ErrPermanent, p.Name, cs.Name, w.Reason, w.Message)
				}
				reasons = append(reasons, fmt.Sprintf("%s: %s", cs.Name, w.Reason))
			}
		}
	}
	sort.Strings(reasons)
	return false, fmt.Sprintf("waiting for %s (%s)", strings.Join(pending, ", "), strings.Join(reasons, "; ")), nil
}

// Delete removes the namespace and, by cascade, everything in it.
func (k *KubeCluster) Delete(ctx context.Context, ns string) error {
	bg := metav1.DeletePropagationBackground
	err := k.cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{PropagationPolicy: &bg})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", ns, err)
	}
	return nil
}

// WaitGone blocks until the namespace no longer exists.
func (k *KubeCluster) WaitGone(ctx context.Context, ns string) error {
	t := time.NewTicker(k.poll)
	defer t.Stop()
	for {
		exists, err := k.Exists(ctx, ns)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace %s still terminating: %w", ns, ctx.Err())
		case <-t.C:
		}
	}
}

// Exists reports whether the namespace exists.
func (k *KubeCluster) Exists(ctx context.Context, ns string) (bool, error) {
	_, err := k.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("get namespace %s: %w", ns, err)
	}
	return true, nil
}

// ListManaged lists namespaces created by the platform.
func (k *KubeCluster) ListManaged(ctx context.Context) ([]ManagedNamespace, error) {
	list, err := k.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: LabelManagedBy + "=" + ManagedByValue})
	if err != nil {
		return nil, fmt.Errorf("list managed namespaces: %w", err)
	}
	out := make([]ManagedNamespace, 0, len(list.Items))
	for _, n := range list.Items {
		out = append(out, ManagedNamespace{
			Name: n.Name, RangeID: n.Labels[LabelRangeID],
			Quarantined: n.Labels[LabelQuarantined] == "true",
			Terminating: n.Status.Phase == corev1.NamespaceTerminating,
		})
	}
	return out, nil
}

// Quarantine labels the namespace and scales its workloads to zero. The
// namespace's default-deny policies stay in force; an operator can
// inspect it before deleting it.
func (k *KubeCluster) Quarantine(ctx context.Context, ns string) error {
	patch := []byte(`{"metadata":{"labels":{"` + LabelQuarantined + `":"true"}}}`)
	if _, err := k.cs.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label namespace %s: %w", ns, err)
	}
	deps, err := k.cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list deployments: %w", err)
	}
	for _, d := range deps.Items {
		if _, err := k.cs.AppsV1().Deployments(ns).Patch(ctx, d.Name, types.MergePatchType, []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("scale %s/%s: %w", ns, d.Name, err)
		}
	}
	return nil
}

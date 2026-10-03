package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestApplyOrderAndIdempotency(t *testing.T) {
	cs := fake.NewClientset()
	var order []string
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, a.GetResource().Resource)
		return false, nil, nil
	})
	k := NewKubeClusterFromClient(cs)
	b := buildTest()

	refs, err := k.Apply(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1+1+1+1+4+1+1+2 {
		t.Fatalf("refs = %d %+v", len(refs), refs)
	}
	firstDeployment, lastPolicy := -1, -1
	for i, r := range order {
		if r == "deployments" && firstDeployment < 0 {
			firstDeployment = i
		}
		if r == "networkpolicies" {
			lastPolicy = i
		}
	}
	if order[0] != "namespaces" || lastPolicy < 0 || firstDeployment < lastPolicy {
		t.Fatalf("network policies must exist before any workload: %v", order)
	}

	// A retry after a partial failure succeeds without duplicates.
	if _, err := k.Apply(context.Background(), b); err != nil {
		t.Fatalf("re-apply must be idempotent: %v", err)
	}
}

func TestApplyRefusesForeignNamespace(t *testing.T) {
	b := buildTest()
	foreign := b.Namespace.DeepCopy()
	foreign.Labels[LabelRangeID] = uuid.NewString()
	k := NewKubeClusterFromClient(fake.NewClientset(foreign))
	if _, err := k.Apply(context.Background(), b); err == nil || !strings.Contains(err.Error(), "another range") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestWaitReady(t *testing.T) {
	ns := "range-x"
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: ns}}
	cs := fake.NewClientset(dep)
	k := NewKubeClusterFromClient(cs)
	k.poll = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	err := k.WaitReady(ctx, ns)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "waiting for web01") {
		t.Fatalf("expected timeout diagnostics, got %v", err)
	}

	dep.Status.ReadyReplicas = 1
	if _, err := cs.AppsV1().Deployments(ns).UpdateStatus(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.WaitReady(context.Background(), ns); err != nil {
		t.Fatalf("ready deployment: %v", err)
	}
}

func TestWaitReadyDetectsPermanentFailures(t *testing.T) {
	ns := "range-y"
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "db01", Namespace: ns}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "db01-abc", Namespace: ns},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "db01", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError", Message: "secret missing"}},
		}}},
	}
	k := NewKubeClusterFromClient(fake.NewClientset(dep, pod))
	if err := k.WaitReady(context.Background(), ns); !errors.Is(err, ErrPermanent) {
		t.Fatalf("expected permanent failure, got %v", err)
	}

	failing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web01", Namespace: "range-z"},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: "pods violate PodSecurity restricted",
		}}},
	}
	k = NewKubeClusterFromClient(fake.NewClientset(failing))
	if err := k.WaitReady(context.Background(), "range-z"); !errors.Is(err, ErrPermanent) || !strings.Contains(err.Error(), "PodSecurity") {
		t.Fatalf("expected admission failure, got %v", err)
	}
}

func TestDeleteListQuarantine(t *testing.T) {
	b := buildTest()
	k := NewKubeClusterFromClient(fake.NewClientset())
	k.poll = 5 * time.Millisecond
	ctx := context.Background()
	if _, err := k.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
	unmanaged := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
	if _, err := k.cs.CoreV1().Namespaces().Create(ctx, unmanaged, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	list, err := k.ListManaged(ctx)
	if err != nil || len(list) != 1 || list[0].Name != b.Namespace.Name || list[0].RangeID == "" {
		t.Fatalf("ListManaged = %+v %v", list, err)
	}

	if err := k.Quarantine(ctx, b.Namespace.Name); err != nil {
		t.Fatal(err)
	}
	list, _ = k.ListManaged(ctx)
	if !list[0].Quarantined {
		t.Fatal("namespace not labelled quarantined")
	}
	deps, _ := k.cs.AppsV1().Deployments(b.Namespace.Name).List(ctx, metav1.ListOptions{})
	for _, d := range deps.Items {
		if *d.Spec.Replicas != 0 {
			t.Fatalf("%s not scaled to zero", d.Name)
		}
	}

	if err := k.Delete(ctx, b.Namespace.Name); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(ctx, b.Namespace.Name); err != nil {
		t.Fatalf("deleting a missing namespace must be a no-op: %v", err)
	}
	if err := k.WaitGone(ctx, b.Namespace.Name); err != nil {
		t.Fatal(err)
	}
	if ok, _ := k.Exists(ctx, b.Namespace.Name); ok {
		t.Fatal("namespace still exists")
	}
}

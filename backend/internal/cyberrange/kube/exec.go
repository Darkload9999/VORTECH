package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecOptions describes a command run in a range workload.
type ExecOptions struct {
	Namespace string
	Workload  string
	Command   []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	TTY       bool
	// Resize delivers terminal sizes for interactive sessions (optional).
	Resize remotecommand.TerminalSizeQueue
}

// Exec runs a command in the running pod of a workload, streaming its
// input and output. It is the primitive behind the player terminal.
func (k *KubeCluster) Exec(ctx context.Context, o ExecOptions) error {
	if k.rest == nil {
		return errors.New("exec requires a real cluster connection")
	}
	pod, err := k.runningPod(ctx, o.Namespace, o.Workload)
	if err != nil {
		return err
	}
	req := k.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(o.Namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: o.Workload, Command: o.Command,
			Stdin: o.Stdin != nil, Stdout: o.Stdout != nil, Stderr: o.Stderr != nil && !o.TTY, TTY: o.TTY,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(k.rest, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("exec %s/%s: %w", o.Namespace, o.Workload, err)
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: o.Stdin, Stdout: o.Stdout, Stderr: o.Stderr, Tty: o.TTY, TerminalSizeQueue: o.Resize,
	})
}

// ExecOutput runs a non-interactive command and returns its output.
func (k *KubeCluster) ExecOutput(ctx context.Context, ns, workload string, cmd ...string) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	err = k.Exec(ctx, ExecOptions{Namespace: ns, Workload: workload, Command: cmd, Stdout: &out, Stderr: &errb})
	return out.String(), errb.String(), err
}

func (k *KubeCluster) runningPod(ctx context.Context, ns, workload string) (string, error) {
	pods, err := k.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: LabelWorkload + "=" + workload})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("workload %s/%s has no running pod", ns, workload)
}

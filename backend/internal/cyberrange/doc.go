// Package cyberrange manages isolated cyber ranges: range templates
// (validated workload specs), the lifecycle state machine (REQUESTED ...
// DESTROYED, FAILED, EXPIRED), capacity accounting and the player-facing
// API. The directory is not named "range" because range is a Go keyword.
//
// Subpackages keep Kubernetes out of the API binary: kube renders and
// applies range namespaces with client-go, and controller (run by the
// worker) executes provisioning jobs, admission, expiry and
// reconciliation.
package cyberrange

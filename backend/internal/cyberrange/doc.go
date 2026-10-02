// Package cyberrange manages isolated cyber ranges: the lifecycle state
// machine (REQUESTED ... DESTROYED, FAILED, EXPIRED), asynchronous
// provisioning jobs, the capacity scheduler, the warm pool and
// reconciliation against K3s. The directory is not named "range" because
// range is a Go keyword.
//
// Implemented in phase 5.
package cyberrange

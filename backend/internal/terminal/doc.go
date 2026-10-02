// Package terminal bridges xterm.js sessions to a PTY inside the player's
// security-workstation via the Kubernetes exec API, after verifying
// identity, range ownership, range state and session expiry. The browser
// never receives Kubernetes credentials.
//
// Implemented in phase 6.
package terminal

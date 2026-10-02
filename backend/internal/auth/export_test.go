package auth

import "time"

// Test-only hooks; compiled only into this package's test binary.

func (s *KeySet) SetMinRefresh(d time.Duration) { s.minRefresh = d }

func (v *Verifier) SetNow(f func() time.Time) { v.now = f }

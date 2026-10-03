package cyberrange

// Resources is an amount of cluster capacity.
type Resources struct {
	CPUMillis  int64 `json:"cpu_millis"`
	MemoryMiB  int64 `json:"memory_mib"`
	StorageMiB int64 `json:"storage_mib"`
}

// Add returns r + o.
func (r Resources) Add(o Resources) Resources {
	return Resources{r.CPUMillis + o.CPUMillis, r.MemoryMiB + o.MemoryMiB, r.StorageMiB + o.StorageMiB}
}

// Capacity is the cluster budget available to ranges.
//
//	available = (cluster − reserved for the platform) × (1 − headroom)
//
// The reserve keeps PostgreSQL, Keycloak, the API and K3s itself alive; the
// headroom absorbs bursts so ranges can never exhaust the VPS.
type Capacity struct {
	Cluster         Resources
	Reserved        Resources
	HeadroomPercent int64
}

// Available is the total capacity ranges may use.
func (c Capacity) Available() Resources {
	f := func(total, reserved int64) int64 {
		v := (total - reserved) * (100 - c.HeadroomPercent) / 100
		if v < 0 {
			return 0
		}
		return v
	}
	return Resources{
		CPUMillis:  f(c.Cluster.CPUMillis, c.Reserved.CPUMillis),
		MemoryMiB:  f(c.Cluster.MemoryMiB, c.Reserved.MemoryMiB),
		StorageMiB: f(c.Cluster.StorageMiB, c.Reserved.StorageMiB),
	}
}

// Fits reports whether request can be admitted given what is in use, and
// which dimension blocks it otherwise ("cpu", "memory" or "storage").
func (c Capacity) Fits(inUse, request Resources) (bool, string) {
	avail := c.Available()
	next := inUse.Add(request)
	switch {
	case next.CPUMillis > avail.CPUMillis:
		return false, "cpu"
	case next.MemoryMiB > avail.MemoryMiB:
		return false, "memory"
	case next.StorageMiB > avail.StorageMiB:
		return false, "storage"
	}
	return true, ""
}

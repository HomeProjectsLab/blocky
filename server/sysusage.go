package server

// sysSnapshot is the latest system-usage sample published by the (linux-only)
// sampler and merged into GET /api/ui/system. It is nil until the first sample,
// and on non-linux the sampler is a no-op so it stays nil — the endpoint then
// omits the fields and the UI header hides itself.
type sysSnapshot struct {
	CPUPerCore   []float64 // per-core busy %, 0..100
	CPUTotal     float64   // mean across cores, 0..100
	MemUsed      uint64    // bytes
	MemTotal     uint64    // bytes
	DiskUsed     uint64    // bytes, filesystem backing the data dir
	DiskTotal    uint64    // bytes
	DiskReadBps  float64   // bytes/sec across physical disks
	DiskWriteBps float64   // bytes/sec across physical disks
}

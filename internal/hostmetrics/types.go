// Package hostmetrics samples host CPU, memory and GPU load plus camfarm's own
// process usage. It is independent of the fleet: it publishes nothing to the
// fleet feed, writes no journal entry and cannot change a replay hash.
package hostmetrics

import "time"

// Sample is one reading. Every section is independent: one that cannot be read
// has Available false and a Reason, and the others are unaffected.
type Sample struct {
	Time    time.Time `json:"time"`
	CPU     CPU       `json:"cpu"`
	Memory  Memory    `json:"memory"`
	GPU     GPU       `json:"gpu"`
	Process Process   `json:"process"`
}

// CPU is host-wide. UsagePercent is null until a baseline exists (the first
// sample after the sampler wakes). LimitCPUs is the cgroup quota, if any.
type CPU struct {
	Available    bool     `json:"available"`
	Reason       string   `json:"reason,omitempty"`
	UsagePercent *float64 `json:"usagePercent"`
	LogicalCPUs  int      `json:"logicalCPUs"`
	Load1        float64  `json:"load1"`
	Load5        float64  `json:"load5"`
	Load15       float64  `json:"load15"`
	LimitCPUs    *float64 `json:"limitCPUs"`
}

// Memory is host-wide. LimitBytes is the cgroup limit, if any.
type Memory struct {
	Available      bool    `json:"available"`
	Reason         string  `json:"reason,omitempty"`
	TotalBytes     uint64  `json:"totalBytes"`
	UsedBytes      uint64  `json:"usedBytes"`
	AvailableBytes uint64  `json:"availableBytes"`
	SwapTotalBytes uint64  `json:"swapTotalBytes"`
	SwapUsedBytes  uint64  `json:"swapUsedBytes"`
	LimitBytes     *uint64 `json:"limitBytes"`
}

// GPU lists the devices nvidia-smi reports. Devices is never null.
type GPU struct {
	Available bool     `json:"available"`
	Reason    string   `json:"reason,omitempty"`
	Devices   []Device `json:"devices"`
}

// Device is one GPU. A value nvidia-smi reports as N/A or Not Supported is null.
// GPUUtilPercent is nvidia-smi's utilization.gpu, the share of time a kernel was
// running, not true SM occupancy (that needs DCGM).
type Device struct {
	Index              int      `json:"index"`
	Name               string   `json:"name"`
	UUID               string   `json:"uuid"`
	GPUUtilPercent     *float64 `json:"gpuUtilPercent"`
	MemUtilPercent     *float64 `json:"memUtilPercent"`
	MemUsedBytes       *uint64  `json:"memUsedBytes"`
	MemTotalBytes      *uint64  `json:"memTotalBytes"`
	EncoderUtilPercent *float64 `json:"encoderUtilPercent"`
	DecoderUtilPercent *float64 `json:"decoderUtilPercent"`
	TemperatureC       *float64 `json:"temperatureC"`
	PowerWatts         *float64 `json:"powerWatts"`
	PowerLimitWatts    *float64 `json:"powerLimitWatts"`
}

// Process is camfarm's own usage. CPUPercent can exceed 100 on multiple cores.
type Process struct {
	Available  bool     `json:"available"`
	Reason     string   `json:"reason,omitempty"`
	CPUPercent *float64 `json:"cpuPercent"`
	RSSBytes   uint64   `json:"rssBytes"`
	Goroutines int      `json:"goroutines"`
	OpenFDs    int      `json:"openFDs"`
}

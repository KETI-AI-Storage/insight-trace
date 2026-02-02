package collector

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"insight-trace/pkg/argo"
	"insight-trace/pkg/types"
)

// MetricsCollector collects real-time metrics from the container
type MetricsCollector struct {
	config     *types.CollectorConfig
	podName    string
	podNamespace string
	containerName string
	nodeName   string

	// Pipeline detection
	isPipeline   bool   // Whether this is a pipeline workload
	pipelineStep string // Current pipeline step (preprocess, train, evaluate, etc.)

	// Current metrics
	currentMetrics *types.ResourceMetrics
	metricsMux     sync.RWMutex

	// Historical data
	metricsHistory []types.ResourceMetrics
	historyMux     sync.RWMutex

	// Previous values for rate calculation
	prevDiskRead   int64
	prevDiskWrite  int64
	prevNetRx      int64
	prevNetTx      int64
	prevTimestamp  time.Time

	// Argo Workflows integration
	argoClient   *argo.ArgoClient
	argoInfo     *types.ArgoWorkflowInfo
	argoInfoMux  sync.RWMutex
	podLabels    map[string]string

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector(config *types.CollectorConfig) *MetricsCollector {
	ctx, cancel := context.WithCancel(context.Background())

	// Check if this is a pipeline workload
	isPipeline := os.Getenv("IS_PIPELINE") == "true"
	pipelineStep := os.Getenv("PIPELINE_STEP")

	mc := &MetricsCollector{
		config:         config,
		podName:        os.Getenv("POD_NAME"),
		podNamespace:   os.Getenv("POD_NAMESPACE"),
		containerName:  os.Getenv("CONTAINER_NAME"),
		nodeName:       os.Getenv("NODE_NAME"),
		isPipeline:     isPipeline,
		pipelineStep:   pipelineStep,
		metricsHistory: make([]types.ResourceMetrics, 0, config.MaxMetricsHistory),
		argoClient:     argo.NewArgoClient(),
		podLabels:      make(map[string]string),
		ctx:            ctx,
		cancel:         cancel,
	}

	// Load pod labels from Downward API if available
	mc.loadPodLabels()

	// Log pipeline detection status
	if mc.isPipeline {
		log.Printf("[Pipeline] Detected as PIPELINE workload, step: %s", mc.pipelineStep)
	} else {
		log.Printf("[Workload] Detected as SIMPLE workload (non-pipeline)")
	}

	return mc
}

// Start begins collecting metrics
func (mc *MetricsCollector) Start() {
	log.Printf("Starting metrics collector for pod %s/%s", mc.podNamespace, mc.podName)

	mc.wg.Add(1)
	go mc.collectLoop()
}

// Stop stops the metrics collector
func (mc *MetricsCollector) Stop() {
	log.Println("Stopping metrics collector...")
	mc.cancel()
	mc.wg.Wait()
	log.Println("Metrics collector stopped")
}

// collectLoop continuously collects metrics
func (mc *MetricsCollector) collectLoop() {
	defer mc.wg.Done()

	ticker := time.NewTicker(mc.config.MetricsInterval)
	defer ticker.Stop()

	// Collect initial metrics
	mc.collectOnce()

	for {
		select {
		case <-mc.ctx.Done():
			return
		case <-ticker.C:
			mc.collectOnce()
		}
	}
}

// collectOnce collects metrics once
func (mc *MetricsCollector) collectOnce() {
	metrics := &types.ResourceMetrics{
		Timestamp: time.Now(),
	}

	// Collect CPU metrics
	mc.collectCPUMetrics(metrics)

	// Collect Memory metrics
	mc.collectMemoryMetrics(metrics)

	// Collect Disk I/O metrics
	mc.collectDiskMetrics(metrics)

	// Collect Network metrics
	mc.collectNetworkMetrics(metrics)

	// Collect GPU metrics (if available)
	mc.collectGPUMetrics(metrics)

	// Calculate rates
	mc.calculateRates(metrics)

	// Store current metrics
	mc.metricsMux.Lock()
	mc.currentMetrics = metrics
	mc.metricsMux.Unlock()

	// Add to history
	mc.addToHistory(*metrics)

	// Update previous values
	mc.prevDiskRead = metrics.DiskReadBytes
	mc.prevDiskWrite = metrics.DiskWriteBytes
	mc.prevNetRx = metrics.NetworkRxBytes
	mc.prevNetTx = metrics.NetworkTxBytes
	mc.prevTimestamp = metrics.Timestamp
}

// collectCPUMetrics collects CPU usage metrics
func (mc *MetricsCollector) collectCPUMetrics(metrics *types.ResourceMetrics) {
	// Try cgroup v2 first
	cpuUsage, err := mc.readCgroupV2CPU()
	if err != nil {
		// Fallback to cgroup v1
		cpuUsage, err = mc.readCgroupV1CPU()
		if err != nil {
			log.Printf("Failed to read CPU metrics: %v", err)
			return
		}
	}
	metrics.CPUUsagePercent = cpuUsage
	metrics.CPUCores = mc.getCPUCores()

	// Read throttled periods
	metrics.CPUThrottledPeriod = mc.readCPUThrottled()
}

// readCgroupV2CPU reads CPU usage from cgroup v2
func (mc *MetricsCollector) readCgroupV2CPU() (float64, error) {
	// Read cpu.stat from cgroup v2
	statPath := "/sys/fs/cgroup/cpu.stat"
	data, err := ioutil.ReadFile(statPath)
	if err != nil {
		return 0, err
	}

	var usageUsec int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "usage_usec") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				usageUsec, _ = strconv.ParseInt(parts[1], 10, 64)
			}
		}
	}

	// Calculate percentage (simplified - needs time delta calculation)
	return float64(usageUsec) / 1000000.0, nil
}

// readCgroupV1CPU reads CPU usage from cgroup v1
func (mc *MetricsCollector) readCgroupV1CPU() (float64, error) {
	// Read cpuacct.usage
	usagePath := "/sys/fs/cgroup/cpu/cpuacct.usage"
	data, err := ioutil.ReadFile(usagePath)
	if err != nil {
		// Try alternative path
		usagePath = "/sys/fs/cgroup/cpuacct/cpuacct.usage"
		data, err = ioutil.ReadFile(usagePath)
		if err != nil {
			return 0, err
		}
	}

	usageNs, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	// Convert nanoseconds to percentage (simplified)
	return float64(usageNs) / 1000000000.0, nil
}

// getCPUCores returns the number of CPU cores available
func (mc *MetricsCollector) getCPUCores() int {
	// Try to read from cgroup cpu.max (v2) or cpu.cfs_quota_us (v1)
	quotaPath := "/sys/fs/cgroup/cpu.max"
	data, err := ioutil.ReadFile(quotaPath)
	if err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 2 && parts[0] != "max" {
			quota, _ := strconv.ParseInt(parts[0], 10, 64)
			period, _ := strconv.ParseInt(parts[1], 10, 64)
			if period > 0 {
				return int(quota / period)
			}
		}
	}

	// Fallback: read from /proc/cpuinfo
	data, err = ioutil.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 1
	}
	return strings.Count(string(data), "processor")
}

// readCPUThrottled reads CPU throttled periods
func (mc *MetricsCollector) readCPUThrottled() int64 {
	// cgroup v2
	statPath := "/sys/fs/cgroup/cpu.stat"
	data, err := ioutil.ReadFile(statPath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "nr_throttled") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					throttled, _ := strconv.ParseInt(parts[1], 10, 64)
					return throttled
				}
			}
		}
	}
	return 0
}

// collectMemoryMetrics collects memory usage metrics
func (mc *MetricsCollector) collectMemoryMetrics(metrics *types.ResourceMetrics) {
	// Try cgroup v2 first
	memCurrent, err := mc.readFile("/sys/fs/cgroup/memory.current")
	if err == nil {
		metrics.MemoryUsageBytes, _ = strconv.ParseInt(strings.TrimSpace(memCurrent), 10, 64)

		memMax, _ := mc.readFile("/sys/fs/cgroup/memory.max")
		if memMax != "max\n" && memMax != "" {
			metrics.MemoryLimitBytes, _ = strconv.ParseInt(strings.TrimSpace(memMax), 10, 64)
		}
	} else {
		// Fallback to cgroup v1
		memUsage, _ := mc.readFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
		metrics.MemoryUsageBytes, _ = strconv.ParseInt(strings.TrimSpace(memUsage), 10, 64)

		memLimit, _ := mc.readFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
		metrics.MemoryLimitBytes, _ = strconv.ParseInt(strings.TrimSpace(memLimit), 10, 64)
	}

	// Read RSS from memory.stat
	statPath := "/sys/fs/cgroup/memory.stat"
	data, err := ioutil.ReadFile(statPath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "anon ") || strings.HasPrefix(line, "rss ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					metrics.MemoryRSSBytes, _ = strconv.ParseInt(parts[1], 10, 64)
					break
				}
			}
		}
	}

	// Calculate percentage
	if metrics.MemoryLimitBytes > 0 {
		metrics.MemoryUsagePercent = float64(metrics.MemoryUsageBytes) / float64(metrics.MemoryLimitBytes) * 100
	}
}

// collectDiskMetrics collects disk I/O metrics
func (mc *MetricsCollector) collectDiskMetrics(metrics *types.ResourceMetrics) {
	// Read from /proc/diskstats or cgroup io.stat
	ioStatPath := "/sys/fs/cgroup/io.stat"
	data, err := ioutil.ReadFile(ioStatPath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" {
				continue
			}
			// Parse: "MAJ:MIN rbytes=X wbytes=Y rios=Z wios=W"
			parts := strings.Fields(line)
			for _, part := range parts {
				if strings.HasPrefix(part, "rbytes=") {
					val, _ := strconv.ParseInt(strings.TrimPrefix(part, "rbytes="), 10, 64)
					metrics.DiskReadBytes += val
				} else if strings.HasPrefix(part, "wbytes=") {
					val, _ := strconv.ParseInt(strings.TrimPrefix(part, "wbytes="), 10, 64)
					metrics.DiskWriteBytes += val
				} else if strings.HasPrefix(part, "rios=") {
					val, _ := strconv.ParseInt(strings.TrimPrefix(part, "rios="), 10, 64)
					metrics.DiskReadOps += val
				} else if strings.HasPrefix(part, "wios=") {
					val, _ := strconv.ParseInt(strings.TrimPrefix(part, "wios="), 10, 64)
					metrics.DiskWriteOps += val
				}
			}
		}
	} else {
		// Fallback to /proc/self/io
		mc.readProcIO(metrics)
	}
}

// readProcIO reads I/O stats from /proc/self/io
func (mc *MetricsCollector) readProcIO(metrics *types.ResourceMetrics) {
	data, err := ioutil.ReadFile("/proc/self/io")
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "read_bytes:") {
			val, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "read_bytes:")), 10, 64)
			metrics.DiskReadBytes = val
		} else if strings.HasPrefix(line, "write_bytes:") {
			val, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "write_bytes:")), 10, 64)
			metrics.DiskWriteBytes = val
		}
	}
}

// collectNetworkMetrics collects network I/O metrics
func (mc *MetricsCollector) collectNetworkMetrics(metrics *types.ResourceMetrics) {
	// Read from /proc/net/dev
	data, err := ioutil.ReadFile("/proc/net/dev")
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		// Skip non-eth interfaces (lo, etc.)
		if !strings.Contains(line, "eth") && !strings.Contains(line, "ens") && !strings.Contains(line, "enp") {
			continue
		}

		// Parse: "iface: rx_bytes rx_packets ... tx_bytes tx_packets ..."
		parts := strings.Fields(line)
		if len(parts) >= 10 {
			rxBytes, _ := strconv.ParseInt(parts[1], 10, 64)
			txBytes, _ := strconv.ParseInt(parts[9], 10, 64)
			metrics.NetworkRxBytes += rxBytes
			metrics.NetworkTxBytes += txBytes
		}
	}
}

// collectGPUMetrics collects GPU metrics from DCGM Exporter
func (mc *MetricsCollector) collectGPUMetrics(metrics *types.ResourceMetrics) {
	// Get DCGM exporter endpoint
	dcgmEndpoint := os.Getenv("DCGM_EXPORTER_ENDPOINT")
	if dcgmEndpoint == "" {
		dcgmEndpoint = "http://dcgm-exporter.gpu-monitoring.svc.cluster.local:9400/metrics"
	}

	// Fetch metrics from DCGM Exporter
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(dcgmEndpoint)
	if err != nil {
		// DCGM not available
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	// Parse Prometheus format and find metrics for this pod
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		// Look for metrics matching this pod
		podFilter := fmt.Sprintf(`pod="%s"`, mc.podName)
		if !strings.Contains(line, podFilter) && !strings.Contains(line, `pod=""`) {
			continue
		}

		// Parse GPU utilization
		if strings.HasPrefix(line, "DCGM_FI_DEV_GPU_UTIL") {
			if val := mc.extractMetricValue(line); val >= 0 {
				metrics.GPUUsagePercent = val
			}
		}

		// Parse GPU memory used (MiB)
		if strings.HasPrefix(line, "DCGM_FI_DEV_FB_USED") {
			if val := mc.extractMetricValue(line); val >= 0 {
				metrics.GPUMemoryUsedMB = int64(val)
			}
		}

		// Parse GPU memory total (MiB)
		if strings.HasPrefix(line, "DCGM_FI_DEV_FB_TOTAL") {
			if val := mc.extractMetricValue(line); val >= 0 {
				metrics.GPUMemoryTotalMB = int64(val)
			}
		}

		// Parse GPU temperature
		if strings.HasPrefix(line, "DCGM_FI_DEV_GPU_TEMP") {
			if val := mc.extractMetricValue(line); val >= 0 {
				metrics.GPUTemperature = val
			}
		}

		// Parse GPU power usage
		if strings.HasPrefix(line, "DCGM_FI_DEV_POWER_USAGE") {
			if val := mc.extractMetricValue(line); val >= 0 {
				metrics.GPUPowerWatts = val
			}
		}
	}
}

// extractMetricValue extracts the numeric value from a Prometheus metric line
func (mc *MetricsCollector) extractMetricValue(line string) float64 {
	// Format: metric_name{labels} value
	parts := strings.Split(line, "}")
	if len(parts) < 2 {
		return -1
	}
	valStr := strings.TrimSpace(parts[len(parts)-1])
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return -1
	}
	return val
}

// calculateRates calculates per-second rates
func (mc *MetricsCollector) calculateRates(metrics *types.ResourceMetrics) {
	if mc.prevTimestamp.IsZero() {
		return
	}

	elapsed := metrics.Timestamp.Sub(mc.prevTimestamp).Seconds()
	if elapsed <= 0 {
		return
	}

	metrics.DiskReadBytesPerSec = float64(metrics.DiskReadBytes-mc.prevDiskRead) / elapsed
	metrics.DiskWriteBytesPerSec = float64(metrics.DiskWriteBytes-mc.prevDiskWrite) / elapsed
	metrics.NetworkRxBytesPerSec = float64(metrics.NetworkRxBytes-mc.prevNetRx) / elapsed
	metrics.NetworkTxBytesPerSec = float64(metrics.NetworkTxBytes-mc.prevNetTx) / elapsed

	// Ensure non-negative rates
	if metrics.DiskReadBytesPerSec < 0 {
		metrics.DiskReadBytesPerSec = 0
	}
	if metrics.DiskWriteBytesPerSec < 0 {
		metrics.DiskWriteBytesPerSec = 0
	}
}

// addToHistory adds metrics to history with size limit
func (mc *MetricsCollector) addToHistory(metrics types.ResourceMetrics) {
	mc.historyMux.Lock()
	defer mc.historyMux.Unlock()

	mc.metricsHistory = append(mc.metricsHistory, metrics)

	// Trim if exceeds max
	if len(mc.metricsHistory) > mc.config.MaxMetricsHistory {
		mc.metricsHistory = mc.metricsHistory[len(mc.metricsHistory)-mc.config.MaxMetricsHistory:]
	}
}

// GetCurrentMetrics returns the current metrics
func (mc *MetricsCollector) GetCurrentMetrics() *types.ResourceMetrics {
	mc.metricsMux.RLock()
	defer mc.metricsMux.RUnlock()
	return mc.currentMetrics
}

// GetMetricsHistory returns historical metrics
func (mc *MetricsCollector) GetMetricsHistory() []types.ResourceMetrics {
	mc.historyMux.RLock()
	defer mc.historyMux.RUnlock()

	result := make([]types.ResourceMetrics, len(mc.metricsHistory))
	copy(result, mc.metricsHistory)
	return result
}

// GetPodInfo returns pod identification
func (mc *MetricsCollector) GetPodInfo() (name, namespace, container, node string) {
	return mc.podName, mc.podNamespace, mc.containerName, mc.nodeName
}

// readFile is a helper to read file contents
func (mc *MetricsCollector) readFile(path string) (string, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DetectProcessInfo detects the main process info for workload classification
// When shareProcessNamespace is enabled, scans all processes in the pod
func (mc *MetricsCollector) DetectProcessInfo() map[string]string {
	info := make(map[string]string)
	var allCmdlines []string

	// Scan all processes in /proc (works with shareProcessNamespace: true)
	procDirs, err := ioutil.ReadDir("/proc")
	if err == nil {
		for _, dir := range procDirs {
			// Only process numeric directories (PIDs)
			if !dir.IsDir() {
				continue
			}
			pid := dir.Name()
			if pid[0] < '0' || pid[0] > '9' {
				continue
			}

			// Read cmdline for this process
			cmdlinePath := filepath.Join("/proc", pid, "cmdline")
			cmdline, err := ioutil.ReadFile(cmdlinePath)
			if err != nil {
				continue
			}

			cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}

			allCmdlines = append(allCmdlines, cmd)

			// Check for AI framework keywords in this process
			cmdLower := strings.ToLower(cmd)

			// Detect Python/ML frameworks
			if strings.Contains(cmdLower, "python") || strings.Contains(cmdLower, "torch") ||
				strings.Contains(cmdLower, "tensorflow") || strings.Contains(cmdLower, "keras") {
				// This is likely the main AI workload process
				info["cmdline"] = cmd

				// Read environment variables for this process
				environPath := filepath.Join("/proc", pid, "environ")
				if environ, err := ioutil.ReadFile(environPath); err == nil {
					for _, env := range strings.Split(string(environ), "\x00") {
						if strings.HasPrefix(env, "FRAMEWORK=") {
							info["framework"] = strings.TrimPrefix(env, "FRAMEWORK=")
						} else if strings.HasPrefix(env, "WORKLOAD_TYPE=") {
							info["workload_type"] = strings.TrimPrefix(env, "WORKLOAD_TYPE=")
						} else if strings.HasPrefix(env, "PIPELINE_STAGE=") {
							info["pipeline_stage"] = strings.TrimPrefix(env, "PIPELINE_STAGE=")
						}
					}
				}
				break // Found the main AI process
			}
		}
	}

	// If no specific AI process found, use combined cmdlines
	if info["cmdline"] == "" && len(allCmdlines) > 0 {
		info["cmdline"] = strings.Join(allCmdlines, " | ")
	}

	// Fallback to /proc/1/cmdline
	if info["cmdline"] == "" {
		if cmdline, err := ioutil.ReadFile("/proc/1/cmdline"); err == nil {
			cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
			info["cmdline"] = strings.TrimSpace(cmd)
		}
	}

	// Check for common ML framework files
	mlFiles := []string{
		"/app/train.py", "/app/inference.py", "/app/preprocess.py",
		"/workspace/train.py", "/workspace/inference.py",
	}
	for _, f := range mlFiles {
		if _, err := os.Stat(f); err == nil {
			info["ml_script"] = filepath.Base(f)
			break
		}
	}

	return info
}

// GetConfig returns the collector configuration
func (mc *MetricsCollector) GetConfig() *types.CollectorConfig {
	return mc.config
}

// loadPodLabels loads pod labels from Kubernetes Downward API
func (mc *MetricsCollector) loadPodLabels() {
	// Try to read labels from Downward API file
	labelsPath := "/etc/podinfo/labels"
	data, err := ioutil.ReadFile(labelsPath)
	if err != nil {
		// Try alternative path
		labelsPath = "/etc/kubernetes/labels"
		data, err = ioutil.ReadFile(labelsPath)
		if err != nil {
			log.Printf("[Argo] Pod labels not available via Downward API")
			return
		}
	}

	// Parse labels (format: key="value")
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.Trim(strings.TrimSpace(parts[1]), "\"")
			mc.podLabels[key] = value
		}
	}

	if len(mc.podLabels) > 0 {
		log.Printf("[Argo] Loaded %d pod labels", len(mc.podLabels))
	}
}

// SetPodLabels allows manually setting pod labels (useful for testing)
func (mc *MetricsCollector) SetPodLabels(labels map[string]string) {
	mc.podLabels = labels
}

// GetPodLabels returns the pod labels
func (mc *MetricsCollector) GetPodLabels() map[string]string {
	return mc.podLabels
}

// IsPipeline returns whether this is a pipeline workload
func (mc *MetricsCollector) IsPipeline() bool {
	return mc.isPipeline
}

// GetPipelineStep returns the current pipeline step
func (mc *MetricsCollector) GetPipelineStep() string {
	return mc.pipelineStep
}

// IsArgoWorkflow checks if this pod is part of an Argo Workflow
func (mc *MetricsCollector) IsArgoWorkflow() bool {
	if mc.argoClient == nil || !mc.argoClient.IsEnabled() {
		return false
	}
	labels := mc.argoClient.DetectArgoLabels(mc.podLabels)
	return labels != nil
}

// FetchArgoWorkflowInfo fetches Argo workflow information for this pod
func (mc *MetricsCollector) FetchArgoWorkflowInfo() (*types.ArgoWorkflowInfo, error) {
	if mc.argoClient == nil || !mc.argoClient.IsEnabled() {
		return nil, fmt.Errorf("argo client not enabled")
	}

	ctx, cancel := context.WithTimeout(mc.ctx, 10*time.Second)
	defer cancel()

	info, err := mc.argoClient.GetWorkflowInfoForPod(ctx, mc.podNamespace, mc.podLabels)
	if err != nil {
		return nil, err
	}

	// Cache the result
	mc.argoInfoMux.Lock()
	mc.argoInfo = info
	mc.argoInfoMux.Unlock()

	return info, nil
}

// GetArgoWorkflowInfo returns cached Argo workflow info
func (mc *MetricsCollector) GetArgoWorkflowInfo() *types.ArgoWorkflowInfo {
	mc.argoInfoMux.RLock()
	defer mc.argoInfoMux.RUnlock()
	return mc.argoInfo
}

// RefreshArgoInfo refreshes Argo workflow information
func (mc *MetricsCollector) RefreshArgoInfo() {
	if !mc.IsArgoWorkflow() {
		return
	}

	info, err := mc.FetchArgoWorkflowInfo()
	if err != nil {
		log.Printf("[Argo] Failed to fetch workflow info: %v", err)
		return
	}

	if info != nil {
		log.Printf("[Argo] Workflow info updated: %s (step: %s, phase: %s)",
			info.WorkflowName, info.NodeName, info.Phase)
	}
}

// GetArgoClient returns the Argo client
func (mc *MetricsCollector) GetArgoClient() *argo.ArgoClient {
	return mc.argoClient
}

// FormatBytes formats bytes to human readable string
func FormatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

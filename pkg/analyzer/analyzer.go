package analyzer

import (
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"insight-trace/pkg/collector"
	"insight-trace/pkg/types"
)

// WorkloadAnalyzer analyzes metrics to detect workload patterns and pipeline stages
type WorkloadAnalyzer struct {
	collector *collector.MetricsCollector
	config    *types.CollectorConfig

	// Current state
	currentTrace    *types.WorkloadTrace
	currentStage    types.PipelineStage
	stageStartTime  time.Time
	stageMetrics    []types.ResourceMetrics
	traceMux        sync.RWMutex

	// Stage transition detection
	stageTransitions []types.StageTransition

	// Detection thresholds
	thresholds *AnalysisThresholds
}

// AnalysisThresholds contains thresholds for pattern detection
type AnalysisThresholds struct {
	// CPU thresholds
	HighCPUPercent     float64 // > this = high CPU usage
	LowCPUPercent      float64 // < this = low CPU usage

	// Memory thresholds
	HighMemoryPercent  float64
	MemoryGrowthRate   float64 // bytes/sec threshold for memory leak detection

	// I/O thresholds
	HighReadRateMBps   float64 // > this = heavy read
	HighWriteRateMBps  float64 // > this = heavy write
	ReadWriteRatioRead float64 // > this = read-heavy (e.g., 0.8)

	// GPU thresholds
	GPUActivePercent   float64 // > this = GPU actively used
	GPUIdlePercent     float64 // < this = GPU idle

	// Stage detection
	StageChangeMinDuration time.Duration // Minimum duration before considering stage change
	PatternStabilityWindow int           // Number of samples to consider for stable pattern
}

// NewWorkloadAnalyzer creates a new workload analyzer
func NewWorkloadAnalyzer(coll *collector.MetricsCollector, config *types.CollectorConfig) *WorkloadAnalyzer {
	podName, podNamespace, containerName, nodeName := coll.GetPodInfo()

	trace := &types.WorkloadTrace{
		TraceID:      generateTraceID(podName, podNamespace),
		PodName:      podName,
		PodNamespace: podNamespace,
		NodeName:     nodeName,
		IsActive:     true,
		StartTime:    time.Now(),
		CurrentSignature: &types.WorkloadSignature{
			PodName:       podName,
			PodNamespace:  podNamespace,
			ContainerName: containerName,
			FirstSeen:     time.Now(),
			LastSeen:      time.Now(),
		},
		StageHistory: make([]types.PipelineStageMetrics, 0),
	}

	wa := &WorkloadAnalyzer{
		collector:        coll,
		config:           config,
		currentTrace:     trace,
		currentStage:     types.StageUnknown,
		stageMetrics:     make([]types.ResourceMetrics, 0),
		stageTransitions: make([]types.StageTransition, 0),
		thresholds:       getDefaultThresholds(),
	}

	// Initialize Argo workflow info if available
	wa.initArgoWorkflowInfo()

	return wa
}

// initArgoWorkflowInfo initializes Argo workflow information
func (wa *WorkloadAnalyzer) initArgoWorkflowInfo() {
	if !wa.collector.IsArgoWorkflow() {
		log.Printf("[Analyzer] Pod is not part of an Argo Workflow")
		return
	}

	log.Printf("[Analyzer] Detected Argo Workflow pod, fetching workflow info...")
	argoInfo, err := wa.collector.FetchArgoWorkflowInfo()
	if err != nil {
		log.Printf("[Analyzer] Failed to fetch Argo workflow info: %v", err)
		return
	}

	if argoInfo != nil {
		wa.currentTrace.ArgoWorkflow = argoInfo
		wa.updateSignatureWithArgoInfo(argoInfo)
		log.Printf("[Analyzer] Argo workflow info loaded: %s (step: %s)",
			argoInfo.WorkflowName, argoInfo.NodeName)
	}
}

// getDefaultThresholds returns default analysis thresholds
func getDefaultThresholds() *AnalysisThresholds {
	return &AnalysisThresholds{
		HighCPUPercent:         70.0,
		LowCPUPercent:          20.0,
		HighMemoryPercent:      80.0,
		MemoryGrowthRate:       10 * 1024 * 1024, // 10 MB/sec
		HighReadRateMBps:       100.0,            // 100 MB/s
		HighWriteRateMBps:      50.0,             // 50 MB/s
		ReadWriteRatioRead:     0.8,
		GPUActivePercent:       30.0,
		GPUIdlePercent:         5.0,
		StageChangeMinDuration: 30 * time.Second,
		PatternStabilityWindow: 10,
	}
}

// Analyze performs analysis on current metrics
func (wa *WorkloadAnalyzer) Analyze() *types.AnalysisResult {
	metrics := wa.collector.GetMetricsHistory()
	if len(metrics) == 0 {
		return nil
	}

	wa.traceMux.Lock()
	defer wa.traceMux.Unlock()

	result := &types.AnalysisResult{
		TraceID:   wa.currentTrace.TraceID,
		Timestamp: time.Now(),
	}

	// Detect workload type
	wa.detectWorkloadType(metrics)

	// Detect I/O pattern
	wa.detectIOPattern(metrics)

	// Detect pipeline stage
	newStage := wa.detectPipelineStage(metrics)
	if newStage != wa.currentStage && wa.currentStage != types.StageUnknown {
		transition := wa.handleStageTransition(newStage)
		result.StageTransition = transition
	}

	// Detect anomalies
	result.Anomalies = wa.detectAnomalies(metrics)

	// Generate recommendations
	result.Recommendations = wa.generateRecommendations()

	// Update signature
	wa.updateSignature(metrics)
	result.Signature = wa.currentTrace.CurrentSignature

	// Add current metrics to stage metrics
	if len(metrics) > 0 {
		wa.stageMetrics = append(wa.stageMetrics, metrics[len(metrics)-1])
	}

	return result
}

// detectWorkloadType determines the AI workload type
func (wa *WorkloadAnalyzer) detectWorkloadType(metrics []types.ResourceMetrics) {
	processInfo := wa.collector.DetectProcessInfo()

	// Check for explicit environment variable
	if wt, ok := processInfo["workload_type"]; ok {
		switch strings.ToLower(wt) {
		case "image", "vision", "cv":
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeImage
		case "text", "nlp", "language":
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeText
		case "audio", "speech":
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeAudio
		case "multimodal":
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeMultimodal
		case "tabular":
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeTabular
		default:
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeUnknown
		}
		wa.currentTrace.CurrentSignature.Confidence = 1.0
		return
	}

	// Detect from command line
	cmdline := processInfo["cmdline"]
	wa.detectWorkloadFromCmdline(cmdline)

	// Detect framework
	wa.detectFramework(processInfo)
}

// detectWorkloadFromCmdline detects workload type from command line
func (wa *WorkloadAnalyzer) detectWorkloadFromCmdline(cmdline string) {
	cmdLower := strings.ToLower(cmdline)

	// Image/Vision keywords
	imageKeywords := []string{"image", "vision", "cv", "cnn", "resnet", "vgg", "yolo", "detectron", "opencv", "pillow"}
	for _, kw := range imageKeywords {
		if strings.Contains(cmdLower, kw) {
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeImage
			wa.currentTrace.CurrentSignature.Confidence = 0.8
			return
		}
	}

	// NLP/Text keywords
	textKeywords := []string{"nlp", "bert", "gpt", "transformer", "tokenizer", "embedding", "language", "text", "llm"}
	for _, kw := range textKeywords {
		if strings.Contains(cmdLower, kw) {
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeText
			wa.currentTrace.CurrentSignature.Confidence = 0.8
			return
		}
	}

	// Audio keywords
	audioKeywords := []string{"audio", "speech", "wav", "whisper", "asr", "tts", "librosa"}
	for _, kw := range audioKeywords {
		if strings.Contains(cmdLower, kw) {
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeAudio
			wa.currentTrace.CurrentSignature.Confidence = 0.8
			return
		}
	}

	// Multimodal keywords
	multimodalKeywords := []string{"multimodal", "clip", "dalle", "stable-diffusion", "llava"}
	for _, kw := range multimodalKeywords {
		if strings.Contains(cmdLower, kw) {
			wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeMultimodal
			wa.currentTrace.CurrentSignature.Confidence = 0.8
			return
		}
	}

	wa.currentTrace.CurrentSignature.DetectedWorkloadType = types.WorkloadTypeUnknown
	wa.currentTrace.CurrentSignature.Confidence = 0.3
}

// detectFramework detects the ML framework being used
func (wa *WorkloadAnalyzer) detectFramework(processInfo map[string]string) {
	cmdline := strings.ToLower(processInfo["cmdline"])

	frameworks := map[string][]string{
		"pytorch":     {"torch", "pytorch", "torchvision", "torchrun"},
		"tensorflow":  {"tensorflow", "tf.", "keras"},
		"jax":         {"jax", "flax"},
		"mxnet":       {"mxnet"},
		"onnx":        {"onnx", "onnxruntime"},
		"huggingface": {"transformers", "huggingface", "hf_"},
		"sklearn":     {"sklearn", "scikit-learn"},
	}

	for framework, keywords := range frameworks {
		for _, kw := range keywords {
			if strings.Contains(cmdline, kw) {
				wa.currentTrace.CurrentSignature.DetectedFramework = framework
				return
			}
		}
	}

	// Check explicit env var
	if fw, ok := processInfo["framework"]; ok {
		wa.currentTrace.CurrentSignature.DetectedFramework = fw
	}
}

// detectIOPattern analyzes I/O patterns from metrics
func (wa *WorkloadAnalyzer) detectIOPattern(metrics []types.ResourceMetrics) {
	if len(metrics) < wa.thresholds.PatternStabilityWindow {
		return
	}

	// Calculate averages for recent metrics
	window := metrics[len(metrics)-wa.thresholds.PatternStabilityWindow:]
	var avgReadRate, avgWriteRate float64
	var totalRead, totalWrite int64

	for _, m := range window {
		avgReadRate += m.DiskReadBytesPerSec
		avgWriteRate += m.DiskWriteBytesPerSec
		totalRead += m.DiskReadBytes
		totalWrite += m.DiskWriteBytes
	}
	avgReadRate /= float64(len(window))
	avgWriteRate /= float64(len(window))

	readMBps := avgReadRate / (1024 * 1024)
	writeMBps := avgWriteRate / (1024 * 1024)

	// Calculate read/write ratio
	totalIO := totalRead + totalWrite
	var readRatio float64
	if totalIO > 0 {
		readRatio = float64(totalRead) / float64(totalIO)
	}

	// Determine pattern
	signature := wa.currentTrace.CurrentSignature

	if readMBps >= wa.thresholds.HighReadRateMBps && readRatio >= wa.thresholds.ReadWriteRatioRead {
		signature.DetectedIOPattern = types.IOPatternSequentialRead
	} else if writeMBps >= wa.thresholds.HighWriteRateMBps {
		signature.DetectedIOPattern = types.IOPatternWriteHeavy
	} else if readRatio >= 0.6 && readRatio <= 0.8 {
		signature.DetectedIOPattern = types.IOPatternBalanced
	} else if wa.detectBurstPattern(window) {
		signature.DetectedIOPattern = types.IOPatternBurstWrite
	} else if readRatio >= 0.9 && readMBps < wa.thresholds.HighReadRateMBps {
		signature.DetectedIOPattern = types.IOPatternRandomRead
	} else {
		signature.DetectedIOPattern = types.IOPatternBalanced
	}
}

// detectBurstPattern detects burst write patterns (e.g., checkpointing)
func (wa *WorkloadAnalyzer) detectBurstPattern(metrics []types.ResourceMetrics) bool {
	if len(metrics) < 3 {
		return false
	}

	// Look for sudden write spikes
	var maxWrite, avgWrite float64
	for _, m := range metrics {
		if m.DiskWriteBytesPerSec > maxWrite {
			maxWrite = m.DiskWriteBytesPerSec
		}
		avgWrite += m.DiskWriteBytesPerSec
	}
	avgWrite /= float64(len(metrics))

	// Burst if max is significantly higher than average
	if avgWrite > 0 && maxWrite/avgWrite > 5.0 {
		return true
	}
	return false
}

// detectPipelineStage determines the current ML pipeline stage
func (wa *WorkloadAnalyzer) detectPipelineStage(metrics []types.ResourceMetrics) types.PipelineStage {
	if len(metrics) < wa.thresholds.PatternStabilityWindow {
		return types.StageUnknown
	}

	// Check for explicit environment variable
	processInfo := wa.collector.DetectProcessInfo()
	if stage, ok := processInfo["pipeline_stage"]; ok {
		switch strings.ToLower(stage) {
		case "preprocessing", "preprocess":
			return types.StagePreprocessing
		case "training", "train":
			return types.StageTraining
		case "evaluation", "eval":
			return types.StageEvaluation
		case "serving", "inference":
			return types.StageServing
		}
	}

	// Analyze resource usage patterns
	window := metrics[len(metrics)-wa.thresholds.PatternStabilityWindow:]
	avgCPU, avgGPU, avgReadRate, avgWriteRate := wa.calculateAverages(window)

	signature := wa.currentTrace.CurrentSignature
	isGPU := signature.IsGPUWorkload || avgGPU > wa.thresholds.GPUActivePercent

	// Stage detection logic based on resource patterns
	// Preprocessing: High CPU, High disk read, Low/No GPU
	if avgCPU > wa.thresholds.HighCPUPercent && avgReadRate > 50*1024*1024 && avgGPU < wa.thresholds.GPUIdlePercent {
		return types.StagePreprocessing
	}

	// Training: High GPU, Moderate CPU, Periodic write bursts (checkpoints)
	if isGPU && avgGPU > wa.thresholds.GPUActivePercent && avgCPU > wa.thresholds.LowCPUPercent {
		// Check for checkpoint pattern
		if wa.detectBurstPattern(window) || avgWriteRate > 10*1024*1024 {
			return types.StageTraining
		}
		return types.StageTraining
	}

	// Evaluation: Moderate GPU, High read, Low write
	if avgGPU > wa.thresholds.GPUIdlePercent && avgGPU < wa.thresholds.HighCPUPercent && avgReadRate > avgWriteRate*3 {
		return types.StageEvaluation
	}

	// Serving/Inference: Variable GPU usage, Low CPU, Request-based I/O
	if avgCPU < wa.thresholds.HighCPUPercent && (isGPU && avgGPU > 0) {
		// Check for request-response pattern (balanced small I/O)
		if avgReadRate < 10*1024*1024 && avgWriteRate < 10*1024*1024 {
			return types.StageServing
		}
	}

	return wa.currentStage
}

// calculateAverages calculates average metrics from a window
func (wa *WorkloadAnalyzer) calculateAverages(metrics []types.ResourceMetrics) (avgCPU, avgGPU, avgReadRate, avgWriteRate float64) {
	for _, m := range metrics {
		avgCPU += m.CPUUsagePercent
		avgGPU += m.GPUUsagePercent
		avgReadRate += m.DiskReadBytesPerSec
		avgWriteRate += m.DiskWriteBytesPerSec
	}
	n := float64(len(metrics))
	return avgCPU / n, avgGPU / n, avgReadRate / n, avgWriteRate / n
}

// handleStageTransition handles a pipeline stage transition
func (wa *WorkloadAnalyzer) handleStageTransition(newStage types.PipelineStage) *types.StageTransition {
	now := time.Now()

	// Finalize current stage metrics
	if wa.currentStage != types.StageUnknown && len(wa.stageMetrics) > 0 {
		stageMetrics := wa.aggregateStageMetrics()
		stageMetrics.EndTime = &now
		stageMetrics.Duration = now.Sub(stageMetrics.StartTime).Seconds()
		wa.currentTrace.StageHistory = append(wa.currentTrace.StageHistory, stageMetrics)
	}

	// Create transition event
	transition := &types.StageTransition{
		TraceID:      wa.currentTrace.TraceID,
		PodName:      wa.currentTrace.PodName,
		PodNamespace: wa.currentTrace.PodNamespace,
		FromStage:    wa.currentStage,
		ToStage:      newStage,
		Timestamp:    now,
		Reason:       wa.getTransitionReason(wa.currentStage, newStage),
	}

	wa.stageTransitions = append(wa.stageTransitions, *transition)

	log.Printf("Stage transition detected: %s -> %s (pod: %s/%s)",
		wa.currentStage, newStage, wa.currentTrace.PodNamespace, wa.currentTrace.PodName)

	// Start new stage
	wa.currentStage = newStage
	wa.stageStartTime = now
	wa.stageMetrics = make([]types.ResourceMetrics, 0)

	return transition
}

// aggregateStageMetrics aggregates metrics for the current stage
func (wa *WorkloadAnalyzer) aggregateStageMetrics() types.PipelineStageMetrics {
	result := types.PipelineStageMetrics{
		Stage:       wa.currentStage,
		StartTime:   wa.stageStartTime,
		SampleCount: len(wa.stageMetrics),
	}

	if len(wa.stageMetrics) == 0 {
		return result
	}

	var totalCPU, totalMem, totalGPU float64
	var totalRead, totalWrite int64

	for _, m := range wa.stageMetrics {
		totalCPU += m.CPUUsagePercent
		totalMem += m.MemoryUsagePercent
		totalGPU += m.GPUUsagePercent
		totalRead += m.DiskReadBytes
		totalWrite += m.DiskWriteBytes

		if m.CPUUsagePercent > result.MaxCPUUsage {
			result.MaxCPUUsage = m.CPUUsagePercent
		}
		if m.MemoryUsagePercent > result.MaxMemoryUsage {
			result.MaxMemoryUsage = m.MemoryUsagePercent
		}
		if m.GPUUsagePercent > result.MaxGPUUsage {
			result.MaxGPUUsage = m.GPUUsagePercent
		}
	}

	n := float64(len(wa.stageMetrics))
	result.AvgCPUUsage = totalCPU / n
	result.AvgMemoryUsage = totalMem / n
	result.AvgGPUUsage = totalGPU / n
	result.TotalReadBytes = totalRead
	result.TotalWriteBytes = totalWrite

	if totalRead+totalWrite > 0 {
		result.ReadWriteRatio = float64(totalRead) / float64(totalRead+totalWrite)
	}

	result.DetectedIOPattern = wa.currentTrace.CurrentSignature.DetectedIOPattern

	return result
}

// getTransitionReason generates a reason for stage transition
func (wa *WorkloadAnalyzer) getTransitionReason(from, to types.PipelineStage) string {
	transitions := map[string]string{
		"preprocessing->training":  "Data loading complete, GPU utilization increased",
		"training->evaluation":     "Training iterations complete, evaluation phase started",
		"evaluation->serving":      "Model evaluation complete, serving mode activated",
		"unknown->preprocessing":   "Initial data loading detected",
		"unknown->training":        "Training process started",
		"unknown->serving":         "Inference service started",
	}

	key := string(from) + "->" + string(to)
	if reason, ok := transitions[key]; ok {
		return reason
	}
	return "Resource usage pattern changed"
}

// detectAnomalies detects anomalies in workload behavior
func (wa *WorkloadAnalyzer) detectAnomalies(metrics []types.ResourceMetrics) []types.Anomaly {
	var anomalies []types.Anomaly

	if len(metrics) < 2 {
		return anomalies
	}

	current := metrics[len(metrics)-1]

	// CPU spike detection
	if current.CPUUsagePercent > 95 {
		anomalies = append(anomalies, types.Anomaly{
			Type:        "cpu_spike",
			Severity:    "high",
			Description: "CPU usage critically high",
			Timestamp:   current.Timestamp,
			Value:       current.CPUUsagePercent,
			Threshold:   95,
		})
	}

	// Memory pressure detection
	if current.MemoryUsagePercent > 90 {
		anomalies = append(anomalies, types.Anomaly{
			Type:        "memory_pressure",
			Severity:    "high",
			Description: "Memory usage critically high, OOM risk",
			Timestamp:   current.Timestamp,
			Value:       current.MemoryUsagePercent,
			Threshold:   90,
		})
	}

	// Memory leak detection (if history available)
	if len(metrics) >= 10 {
		memGrowth := wa.detectMemoryGrowth(metrics[len(metrics)-10:])
		if memGrowth > wa.thresholds.MemoryGrowthRate {
			anomalies = append(anomalies, types.Anomaly{
				Type:        "memory_leak",
				Severity:    "medium",
				Description: "Possible memory leak detected",
				Timestamp:   current.Timestamp,
				Value:       memGrowth,
				Threshold:   wa.thresholds.MemoryGrowthRate,
			})
		}
	}

	// I/O bottleneck detection
	if current.DiskReadBytesPerSec > 0 && current.GPUUsagePercent > 0 {
		// GPU waiting for data = I/O bottleneck
		if current.GPUUsagePercent < 50 && wa.currentStage == types.StageTraining {
			anomalies = append(anomalies, types.Anomaly{
				Type:        "io_bottleneck",
				Severity:    "medium",
				Description: "GPU underutilized, possible I/O bottleneck",
				Timestamp:   current.Timestamp,
				Value:       current.GPUUsagePercent,
				Threshold:   50,
			})
		}
	}

	// GPU thermal throttling
	if current.GPUTemperature > 85 {
		anomalies = append(anomalies, types.Anomaly{
			Type:        "gpu_thermal",
			Severity:    "high",
			Description: "GPU temperature high, possible thermal throttling",
			Timestamp:   current.Timestamp,
			Value:       current.GPUTemperature,
			Threshold:   85,
		})
	}

	return anomalies
}

// detectMemoryGrowth calculates memory growth rate
func (wa *WorkloadAnalyzer) detectMemoryGrowth(metrics []types.ResourceMetrics) float64 {
	if len(metrics) < 2 {
		return 0
	}

	first := metrics[0]
	last := metrics[len(metrics)-1]

	elapsed := last.Timestamp.Sub(first.Timestamp).Seconds()
	if elapsed <= 0 {
		return 0
	}

	growth := float64(last.MemoryUsageBytes-first.MemoryUsageBytes) / elapsed
	return math.Max(0, growth)
}

// generateRecommendations generates storage recommendations based on analysis
func (wa *WorkloadAnalyzer) generateRecommendations() []types.Recommendation {
	var recommendations []types.Recommendation
	signature := wa.currentTrace.CurrentSignature

	// Storage class recommendation based on I/O pattern
	var recommendedClass string
	var reason string

	switch signature.DetectedIOPattern {
	case types.IOPatternSequentialRead:
		recommendedClass = "high-throughput"
		reason = "Sequential read pattern detected, optimized for streaming data"
	case types.IOPatternRandomRead:
		recommendedClass = "high-iops"
		reason = "Random read pattern detected, optimized for IOPS"
	case types.IOPatternBurstWrite:
		recommendedClass = "high-throughput"
		reason = "Burst write pattern (checkpointing) detected"
	case types.IOPatternWriteHeavy:
		recommendedClass = "high-throughput"
		reason = "Write-heavy workload detected"
	default:
		recommendedClass = "balanced"
		reason = "Balanced I/O pattern"
	}

	if signature.RecommendedStorageClass != recommendedClass {
		recommendations = append(recommendations, types.Recommendation{
			Type:        "storage_class",
			Current:     signature.RecommendedStorageClass,
			Recommended: recommendedClass,
			Reason:      reason,
			Priority:    "medium",
		})
		signature.RecommendedStorageClass = recommendedClass
	}

	// Storage size recommendation based on workload type
	var recommendedSize string
	switch signature.DetectedWorkloadType {
	case types.WorkloadTypeImage:
		recommendedSize = "500Gi" // Large image datasets
	case types.WorkloadTypeText:
		recommendedSize = "200Gi" // Text/NLP models
	case types.WorkloadTypeAudio:
		recommendedSize = "300Gi" // Audio datasets
	case types.WorkloadTypeMultimodal:
		recommendedSize = "1Ti" // Large multimodal data
	default:
		recommendedSize = "200Gi"
	}
	signature.RecommendedStorageSize = recommendedSize

	// IOPS and throughput recommendations
	switch signature.DetectedIOPattern {
	case types.IOPatternSequentialRead, types.IOPatternWriteHeavy:
		signature.RecommendedThroughput = 800 // MB/s
		signature.RecommendedIOPS = 3000
	case types.IOPatternRandomRead, types.IOPatternBurstWrite:
		signature.RecommendedThroughput = 400
		signature.RecommendedIOPS = 10000
	default:
		signature.RecommendedThroughput = 500
		signature.RecommendedIOPS = 5000
	}

	// Cache tier recommendation for hot data
	if wa.currentStage == types.StageTraining && signature.DetectedIOPattern == types.IOPatternSequentialRead {
		recommendations = append(recommendations, types.Recommendation{
			Type:        "cache_tier",
			Current:     "none",
			Recommended: "nvme",
			Reason:      "Training workload with sequential reads benefits from NVMe caching",
			Priority:    "high",
		})
	}

	return recommendations
}

// updateSignature updates the workload signature with current analysis
func (wa *WorkloadAnalyzer) updateSignature(metrics []types.ResourceMetrics) {
	signature := wa.currentTrace.CurrentSignature
	signature.DetectedStage = wa.currentStage
	signature.LastSeen = time.Now()
	signature.UpdatedAt = time.Now()

	// Check if GPU workload
	if len(metrics) > 0 {
		lastMetric := metrics[len(metrics)-1]
		signature.IsGPUWorkload = lastMetric.GPUUsagePercent > 0 || lastMetric.GPUMemoryUsedMB > 0
	}

	// Update Argo info if available
	if argoInfo := wa.collector.GetArgoWorkflowInfo(); argoInfo != nil {
		wa.updateSignatureWithArgoInfo(argoInfo)
	}
}

// updateSignatureWithArgoInfo updates signature with Argo workflow information
func (wa *WorkloadAnalyzer) updateSignatureWithArgoInfo(argoInfo *types.ArgoWorkflowInfo) {
	if argoInfo == nil {
		return
	}

	signature := wa.currentTrace.CurrentSignature

	// Basic workflow info
	signature.ArgoWorkflowName = argoInfo.WorkflowName
	signature.ArgoStepName = argoInfo.NodeName
	signature.ArgoPhase = argoInfo.Phase

	// DAG dependencies
	signature.ArgoDependencies = argoInfo.Dependencies
	signature.ArgoNextSteps = argoInfo.NextSteps

	// Dataset info from artifacts
	if len(argoInfo.InputArtifacts) > 0 {
		signature.ArgoDatasetInputs = make([]string, 0, len(argoInfo.InputArtifacts))
		for _, art := range argoInfo.InputArtifacts {
			if art.S3Key != "" {
				signature.ArgoDatasetInputs = append(signature.ArgoDatasetInputs, art.S3Key)
			} else if art.Path != "" {
				signature.ArgoDatasetInputs = append(signature.ArgoDatasetInputs, art.Path)
			} else if art.From != "" {
				signature.ArgoDatasetInputs = append(signature.ArgoDatasetInputs, art.From)
			}
		}
	}

	if len(argoInfo.OutputArtifacts) > 0 {
		signature.ArgoDatasetOutputs = make([]string, 0, len(argoInfo.OutputArtifacts))
		for _, art := range argoInfo.OutputArtifacts {
			if art.S3Key != "" {
				signature.ArgoDatasetOutputs = append(signature.ArgoDatasetOutputs, art.S3Key)
			} else if art.Path != "" {
				signature.ArgoDatasetOutputs = append(signature.ArgoDatasetOutputs, art.Path)
			}
		}
	}

	// Historical data
	signature.ArgoExecutionCount = argoInfo.ExecutionCount
	signature.ArgoAvgDurationSec = argoInfo.HistoricalAvgDuration

	// Try to infer workload type from Argo parameters/artifacts
	wa.inferWorkloadTypeFromArgo(argoInfo)
}

// inferWorkloadTypeFromArgo tries to infer workload type from Argo metadata
func (wa *WorkloadAnalyzer) inferWorkloadTypeFromArgo(argoInfo *types.ArgoWorkflowInfo) {
	signature := wa.currentTrace.CurrentSignature

	// If already detected, don't override
	if signature.DetectedWorkloadType != types.WorkloadTypeUnknown {
		return
	}

	// Check template name for hints
	templateLower := strings.ToLower(argoInfo.TemplateName)
	nodeLower := strings.ToLower(argoInfo.NodeName)

	// Image-related keywords
	imageKeywords := []string{"image", "vision", "cv", "cnn", "resnet", "yolo", "detectron", "segmentation"}
	for _, kw := range imageKeywords {
		if strings.Contains(templateLower, kw) || strings.Contains(nodeLower, kw) {
			signature.DetectedWorkloadType = types.WorkloadTypeImage
			signature.Confidence = 0.7
			log.Printf("[Analyzer] Inferred workload type from Argo: image (template/node name)")
			return
		}
	}

	// Text/NLP keywords
	textKeywords := []string{"nlp", "bert", "gpt", "transformer", "text", "tokenize", "llm", "embedding"}
	for _, kw := range textKeywords {
		if strings.Contains(templateLower, kw) || strings.Contains(nodeLower, kw) {
			signature.DetectedWorkloadType = types.WorkloadTypeText
			signature.Confidence = 0.7
			log.Printf("[Analyzer] Inferred workload type from Argo: text (template/node name)")
			return
		}
	}

	// Audio keywords
	audioKeywords := []string{"audio", "speech", "whisper", "asr", "tts", "voice"}
	for _, kw := range audioKeywords {
		if strings.Contains(templateLower, kw) || strings.Contains(nodeLower, kw) {
			signature.DetectedWorkloadType = types.WorkloadTypeAudio
			signature.Confidence = 0.7
			log.Printf("[Analyzer] Inferred workload type from Argo: audio (template/node name)")
			return
		}
	}

	// Check input parameters for hints
	if argoInfo.InputParameters != nil {
		for key, value := range argoInfo.InputParameters {
			keyLower := strings.ToLower(key)
			valueLower := strings.ToLower(value)

			if strings.Contains(keyLower, "model") || strings.Contains(keyLower, "dataset") {
				// Check value for type hints
				combined := keyLower + " " + valueLower
				for _, kw := range imageKeywords {
					if strings.Contains(combined, kw) {
						signature.DetectedWorkloadType = types.WorkloadTypeImage
						signature.Confidence = 0.6
						return
					}
				}
				for _, kw := range textKeywords {
					if strings.Contains(combined, kw) {
						signature.DetectedWorkloadType = types.WorkloadTypeText
						signature.Confidence = 0.6
						return
					}
				}
			}
		}
	}

	// Check artifact paths for hints
	for _, art := range argoInfo.InputArtifacts {
		pathLower := strings.ToLower(art.Path + art.S3Key)
		if strings.Contains(pathLower, "image") || strings.Contains(pathLower, "jpg") || strings.Contains(pathLower, "png") {
			signature.DetectedWorkloadType = types.WorkloadTypeImage
			signature.Confidence = 0.5
			return
		}
		if strings.Contains(pathLower, "text") || strings.Contains(pathLower, "corpus") || strings.Contains(pathLower, "vocab") {
			signature.DetectedWorkloadType = types.WorkloadTypeText
			signature.Confidence = 0.5
			return
		}
	}
}

// RefreshArgoInfo refreshes Argo workflow info (call periodically)
func (wa *WorkloadAnalyzer) RefreshArgoInfo() {
	wa.collector.RefreshArgoInfo()

	if argoInfo := wa.collector.GetArgoWorkflowInfo(); argoInfo != nil {
		wa.traceMux.Lock()
		wa.currentTrace.ArgoWorkflow = argoInfo
		wa.updateSignatureWithArgoInfo(argoInfo)
		wa.traceMux.Unlock()
	}
}

// GetCurrentTrace returns the current workload trace
func (wa *WorkloadAnalyzer) GetCurrentTrace() *types.WorkloadTrace {
	wa.traceMux.RLock()
	defer wa.traceMux.RUnlock()
	return wa.currentTrace
}

// GetCurrentSignature returns the current workload signature
func (wa *WorkloadAnalyzer) GetCurrentSignature() *types.WorkloadSignature {
	wa.traceMux.RLock()
	defer wa.traceMux.RUnlock()
	return wa.currentTrace.CurrentSignature
}

// GetStageHistory returns pipeline stage history
func (wa *WorkloadAnalyzer) GetStageHistory() []types.PipelineStageMetrics {
	wa.traceMux.RLock()
	defer wa.traceMux.RUnlock()

	result := make([]types.PipelineStageMetrics, len(wa.currentTrace.StageHistory))
	copy(result, wa.currentTrace.StageHistory)
	return result
}

// generateTraceID generates a unique trace ID
func generateTraceID(podName, namespace string) string {
	timestamp := time.Now().UnixNano()
	return strings.ToLower(strings.ReplaceAll(
		strings.ReplaceAll(
			strings.Join([]string{namespace, podName, string(rune(timestamp % 100000))}, "-"),
			"_", "-"),
		" ", ""))
}

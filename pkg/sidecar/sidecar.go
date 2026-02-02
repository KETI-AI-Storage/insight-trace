package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"insight-trace/pkg/analyzer"
	"insight-trace/pkg/apollo"
	"insight-trace/pkg/collector"
	traceGrpc "insight-trace/pkg/grpc"
	"insight-trace/pkg/types"

	"github.com/gin-gonic/gin"
)

// Sidecar represents the Insight Trace sidecar container
type Sidecar struct {
	config    *types.CollectorConfig
	collector *collector.MetricsCollector
	analyzer  *analyzer.WorkloadAnalyzer

	// HTTP server for local queries
	server *http.Server
	port   string

	// gRPC server
	grpcServer *traceGrpc.TraceServer
	grpcPort   string

	// APOLLO client
	apolloClient   *apollo.Client
	apolloEndpoint string

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewSidecar creates a new sidecar instance
func NewSidecar(config *types.CollectorConfig) *Sidecar {
	ctx, cancel := context.WithCancel(context.Background())

	coll := collector.NewMetricsCollector(config)
	anal := analyzer.NewWorkloadAnalyzer(coll, config)

	port := os.Getenv("SIDECAR_PORT")
	if port == "" {
		port = "9090"
	}

	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = "9091"
	}

	// APOLLO endpoint (gRPC)
	apolloEndpoint := os.Getenv("APOLLO_ENDPOINT")
	if apolloEndpoint == "" {
		apolloEndpoint = "apollo-policy-server.keti.svc.cluster.local:50051"
	}

	// Create gRPC server
	grpcServer := traceGrpc.NewTraceServer(coll, anal, grpcPort)

	// Create APOLLO client
	var apolloClient *apollo.Client
	if apolloEndpoint != "" {
		apolloClient = apollo.NewClient(apolloEndpoint)
	}

	return &Sidecar{
		config:         config,
		collector:      coll,
		analyzer:       anal,
		port:           port,
		grpcServer:     grpcServer,
		grpcPort:       grpcPort,
		apolloClient:   apolloClient,
		apolloEndpoint: apolloEndpoint,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Start starts the sidecar
func (s *Sidecar) Start() error {
	log.Println("Starting Insight Trace Sidecar...")

	// Connect to APOLLO
	if s.apolloClient != nil {
		go func() {
			// Retry connection in background
			for i := 0; i < 5; i++ {
				if err := s.apolloClient.Connect(); err != nil {
					log.Printf("[APOLLO] Connection attempt %d failed: %v", i+1, err)
					time.Sleep(5 * time.Second)
					continue
				}
				log.Printf("[APOLLO] Connected to %s", s.apolloEndpoint)
				break
			}
		}()
	}

	// Start metrics collector
	s.collector.Start()

	// Start analyzer loop
	s.wg.Add(1)
	go s.analyzeLoop()

	// Start reporter loop (send to orchestrator and APOLLO)
	s.wg.Add(1)
	go s.reportLoop()

	// Start HTTP server (REST API)
	s.wg.Add(1)
	go s.startHTTPServer()

	// Start gRPC server
	if err := s.grpcServer.Start(); err != nil {
		log.Printf("Warning: Failed to start gRPC server: %v", err)
	}

	log.Printf("Insight Trace Sidecar started - HTTP: %s, gRPC: %s, APOLLO: %s", s.port, s.grpcPort, s.apolloEndpoint)
	return nil
}

// Stop stops the sidecar gracefully
func (s *Sidecar) Stop() {
	log.Println("Stopping Insight Trace Sidecar...")
	s.cancel()

	// Shutdown HTTP server
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}

	// Close APOLLO client
	if s.apolloClient != nil {
		s.apolloClient.Close()
	}

	// Stop collector
	s.collector.Stop()

	s.wg.Wait()
	log.Println("Insight Trace Sidecar stopped")
}

// Run runs the sidecar until interrupted
func (s *Sidecar) Run() error {
	if err := s.Start(); err != nil {
		return err
	}

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	s.Stop()
	return nil
}

// analyzeLoop periodically analyzes metrics
func (s *Sidecar) analyzeLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.AnalysisInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			result := s.analyzer.Analyze()
			if result != nil {
				s.handleAnalysisResult(result)
			}
		}
	}
}

// handleAnalysisResult processes analysis results
func (s *Sidecar) handleAnalysisResult(result *types.AnalysisResult) {
	// Log stage transitions
	if result.StageTransition != nil {
		log.Printf("Stage transition: %s -> %s",
			result.StageTransition.FromStage,
			result.StageTransition.ToStage)
	}

	// Log anomalies
	for _, anomaly := range result.Anomalies {
		log.Printf("Anomaly detected: %s (%s) - %s",
			anomaly.Type, anomaly.Severity, anomaly.Description)
	}

	// Log high-priority recommendations
	for _, rec := range result.Recommendations {
		if rec.Priority == "high" {
			log.Printf("Recommendation: %s - %s (current: %s, recommended: %s)",
				rec.Type, rec.Reason, rec.Current, rec.Recommended)
		}
	}
}

// reportLoop periodically reports to the orchestrator and APOLLO
func (s *Sidecar) reportLoop() {
	defer s.wg.Done()

	// Wait for initial data collection
	time.Sleep(s.config.MetricsInterval * 2)

	ticker := time.NewTicker(s.config.ReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// Report to legacy orchestrator (HTTP)
			s.reportToOrchestrator()
			// Report to APOLLO (gRPC)
			s.reportToApollo()
		}
	}
}

// reportToOrchestrator sends current signature to the orchestrator
func (s *Sidecar) reportToOrchestrator() {
	if s.config.OrchestratorEndpoint == "" {
		return
	}

	signature := s.analyzer.GetCurrentSignature()
	if signature == nil {
		return
	}

	// Create report payload
	report := map[string]interface{}{
		"trace_id":      s.analyzer.GetCurrentTrace().TraceID,
		"pod_name":      signature.PodName,
		"pod_namespace": signature.PodNamespace,
		"signature":     signature,
		"timestamp":     time.Now(),
	}

	jsonData, err := json.Marshal(report)
	if err != nil {
		log.Printf("Failed to marshal report: %v", err)
		return
	}

	// Send to orchestrator via HTTP
	// Ensure http:// prefix and use port 8080 for REST API
	endpoint := s.config.OrchestratorEndpoint
	// Replace gRPC port with HTTP port
	endpoint = strings.Replace(endpoint, ":50051", ":8080", 1)
	// Add http:// prefix if not present
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}
	url := fmt.Sprintf("%s/api/v1/insight/report", endpoint)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Failed to report to orchestrator: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		log.Printf("Orchestrator returned status: %d", resp.StatusCode)
	}
}

// reportToApollo sends current signature to APOLLO via gRPC
func (s *Sidecar) reportToApollo() {
	if s.apolloClient == nil {
		return
	}

	signature := s.analyzer.GetCurrentSignature()
	if signature == nil {
		return
	}

	// Get pod info
	podName, namespace, containerName, nodeName := s.collector.GetPodInfo()

	// Convert types.WorkloadSignature to apollo.WorkloadSignatureData
	sigData := &apollo.WorkloadSignatureData{
		PodName:       podName,
		PodNamespace:  namespace,
		PodUID:        "", // Will be populated if available
		NodeName:      nodeName,
		ContainerName: containerName,

		WorkloadType: mapWorkloadType(signature.DetectedWorkloadType),
		CurrentStage: mapPipelineStage(signature.DetectedStage),
		IOPattern:    mapIOPattern(signature.DetectedIOPattern),
		Confidence:   signature.Confidence,

		Framework:        signature.DetectedFramework,
		FrameworkVersion: signature.DetectedVersion,

		IsGPUWorkload:      signature.IsGPUWorkload,
		IsDistributed:      signature.IsDistributed,
		IsPipeline:         signature.IsPipeline,
		PipelineStep:       signature.PipelineStep,
		EstimatedBatchSize: signature.EstimatedBatchSize,

		FirstSeen: signature.FirstSeen,
		LastSeen:  signature.LastSeen,
	}

	// Add current metrics if available
	currentMetrics := s.collector.GetCurrentMetrics()
	if currentMetrics != nil {
		sigData.CurrentMetrics = &apollo.ResourceMetricsData{
			CPUUsagePercent:    currentMetrics.CPUUsagePercent,
			MemoryUsagePercent: currentMetrics.MemoryUsagePercent,
			MemoryUsedBytes:    currentMetrics.MemoryUsageBytes,
			GPUUsagePercent:    currentMetrics.GPUUsagePercent,
			GPUMemoryUsedBytes: currentMetrics.GPUMemoryUsedMB * 1024 * 1024,
			DiskReadBytes:      currentMetrics.DiskReadBytes,
			DiskWriteBytes:     currentMetrics.DiskWriteBytes,
			DiskReadIOPS:       currentMetrics.DiskReadOps,
			DiskWriteIOPS:      currentMetrics.DiskWriteOps,
			NetworkRxBytes:     currentMetrics.NetworkRxBytes,
			NetworkTxBytes:     currentMetrics.NetworkTxBytes,
		}
	}

	// Add storage recommendation if available
	if signature.RecommendedStorageClass != "" {
		sigData.StorageRecommendation = &apollo.StorageRecommendationData{
			RecommendedClass: signature.RecommendedStorageClass,
			RecommendedIOPS:  int(signature.RecommendedIOPS),
			Reason:           "Detected from workload analysis",
		}
	}

	// Send to APOLLO
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	if err := s.apolloClient.ReportWorkloadSignature(ctx, sigData); err != nil {
		log.Printf("[APOLLO] Failed to report signature: %v", err)
	}
}

// mapWorkloadType converts types.WorkloadType to apollo.WorkloadType (int32)
func mapWorkloadType(wt types.WorkloadType) int32 {
	switch wt {
	case types.WorkloadTypeImage:
		return 1 // WORKLOAD_TYPE_IMAGE
	case types.WorkloadTypeText:
		return 2 // WORKLOAD_TYPE_TEXT
	case types.WorkloadTypeTabular:
		return 3 // WORKLOAD_TYPE_TABULAR
	case types.WorkloadTypeAudio:
		return 4 // WORKLOAD_TYPE_AUDIO
	case types.WorkloadTypeMultimodal:
		return 6 // WORKLOAD_TYPE_MULTIMODAL
	default:
		return 0 // WORKLOAD_TYPE_UNKNOWN
	}
}

// mapPipelineStage converts types.PipelineStage to apollo.PipelineStage (int32)
func mapPipelineStage(ps types.PipelineStage) int32 {
	switch ps {
	case types.StagePreprocessing:
		return 2 // PIPELINE_STAGE_PREPROCESSING
	case types.StageTraining:
		return 3 // PIPELINE_STAGE_TRAINING
	case types.StageEvaluation:
		return 4 // PIPELINE_STAGE_VALIDATION
	case types.StageServing:
		return 5 // PIPELINE_STAGE_INFERENCE
	default:
		return 0 // PIPELINE_STAGE_UNKNOWN
	}
}

// mapIOPattern converts types.IOPattern to apollo.IOPattern (int32)
func mapIOPattern(iop types.IOPattern) int32 {
	switch iop {
	case types.IOPatternSequentialRead:
		return 4 // IO_PATTERN_SEQUENTIAL
	case types.IOPatternRandomRead:
		return 5 // IO_PATTERN_RANDOM
	case types.IOPatternBurstWrite:
		return 6 // IO_PATTERN_BURSTY
	case types.IOPatternBalanced:
		return 3 // IO_PATTERN_BALANCED
	case types.IOPatternWriteHeavy:
		return 2 // IO_PATTERN_WRITE_HEAVY
	default:
		return 0 // IO_PATTERN_UNKNOWN
	}
}

// startHTTPServer starts the HTTP server for local queries
func (s *Sidecar) startHTTPServer() {
	defer s.wg.Done()

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// Health check
	router.GET("/health", s.handleHealth)

	// Metrics endpoints
	router.GET("/metrics", s.handleMetrics)
	router.GET("/metrics/current", s.handleCurrentMetrics)
	router.GET("/metrics/history", s.handleMetricsHistory)

	// Analysis endpoints
	router.GET("/signature", s.handleSignature)
	router.GET("/trace", s.handleTrace)
	router.GET("/stages", s.handleStages)

	// Prometheus metrics endpoint
	router.GET("/prometheus", s.handlePrometheus)

	s.server = &http.Server{
		Addr:    ":" + s.port,
		Handler: router,
	}

	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("HTTP server error: %v", err)
	}
}

// handleHealth handles health check requests
func (s *Sidecar) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"component": "insight-trace-sidecar",
		"timestamp": time.Now(),
	})
}

// handleMetrics handles metrics summary request
func (s *Sidecar) handleMetrics(c *gin.Context) {
	current := s.collector.GetCurrentMetrics()
	signature := s.analyzer.GetCurrentSignature()

	c.JSON(http.StatusOK, gin.H{
		"current_metrics":  current,
		"current_stage":    signature.DetectedStage,
		"workload_type":    signature.DetectedWorkloadType,
		"io_pattern":       signature.DetectedIOPattern,
		"is_gpu_workload":  signature.IsGPUWorkload,
		"framework":        signature.DetectedFramework,
		"recommendations": gin.H{
			"storage_class": signature.RecommendedStorageClass,
			"storage_size":  signature.RecommendedStorageSize,
			"iops":          signature.RecommendedIOPS,
			"throughput":    signature.RecommendedThroughput,
		},
	})
}

// handleCurrentMetrics handles current metrics request
func (s *Sidecar) handleCurrentMetrics(c *gin.Context) {
	metrics := s.collector.GetCurrentMetrics()
	if metrics == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "metrics not yet available"})
		return
	}
	c.JSON(http.StatusOK, metrics)
}

// handleMetricsHistory handles metrics history request
func (s *Sidecar) handleMetricsHistory(c *gin.Context) {
	history := s.collector.GetMetricsHistory()
	c.JSON(http.StatusOK, gin.H{
		"count":   len(history),
		"metrics": history,
	})
}

// handleSignature handles workload signature request
func (s *Sidecar) handleSignature(c *gin.Context) {
	signature := s.analyzer.GetCurrentSignature()
	c.JSON(http.StatusOK, signature)
}

// handleTrace handles full trace request
func (s *Sidecar) handleTrace(c *gin.Context) {
	trace := s.analyzer.GetCurrentTrace()
	c.JSON(http.StatusOK, trace)
}

// handleStages handles stage history request
func (s *Sidecar) handleStages(c *gin.Context) {
	stages := s.analyzer.GetStageHistory()
	c.JSON(http.StatusOK, gin.H{
		"current_stage": s.analyzer.GetCurrentSignature().DetectedStage,
		"history":       stages,
	})
}

// handlePrometheus handles Prometheus metrics format
func (s *Sidecar) handlePrometheus(c *gin.Context) {
	metrics := s.collector.GetCurrentMetrics()
	signature := s.analyzer.GetCurrentSignature()

	podName, namespace, container, node := s.collector.GetPodInfo()
	labels := fmt.Sprintf(`pod="%s",namespace="%s",container="%s",node="%s"`, podName, namespace, container, node)

	var output string

	if metrics != nil {
		output += fmt.Sprintf("# HELP insight_cpu_usage_percent CPU usage percentage\n")
		output += fmt.Sprintf("# TYPE insight_cpu_usage_percent gauge\n")
		output += fmt.Sprintf("insight_cpu_usage_percent{%s} %f\n", labels, metrics.CPUUsagePercent)

		output += fmt.Sprintf("# HELP insight_memory_usage_percent Memory usage percentage\n")
		output += fmt.Sprintf("# TYPE insight_memory_usage_percent gauge\n")
		output += fmt.Sprintf("insight_memory_usage_percent{%s} %f\n", labels, metrics.MemoryUsagePercent)

		output += fmt.Sprintf("# HELP insight_memory_usage_bytes Memory usage in bytes\n")
		output += fmt.Sprintf("# TYPE insight_memory_usage_bytes gauge\n")
		output += fmt.Sprintf("insight_memory_usage_bytes{%s} %d\n", labels, metrics.MemoryUsageBytes)

		output += fmt.Sprintf("# HELP insight_disk_read_bytes_per_sec Disk read rate\n")
		output += fmt.Sprintf("# TYPE insight_disk_read_bytes_per_sec gauge\n")
		output += fmt.Sprintf("insight_disk_read_bytes_per_sec{%s} %f\n", labels, metrics.DiskReadBytesPerSec)

		output += fmt.Sprintf("# HELP insight_disk_write_bytes_per_sec Disk write rate\n")
		output += fmt.Sprintf("# TYPE insight_disk_write_bytes_per_sec gauge\n")
		output += fmt.Sprintf("insight_disk_write_bytes_per_sec{%s} %f\n", labels, metrics.DiskWriteBytesPerSec)

		if metrics.GPUUsagePercent > 0 {
			output += fmt.Sprintf("# HELP insight_gpu_usage_percent GPU usage percentage\n")
			output += fmt.Sprintf("# TYPE insight_gpu_usage_percent gauge\n")
			output += fmt.Sprintf("insight_gpu_usage_percent{%s} %f\n", labels, metrics.GPUUsagePercent)

			output += fmt.Sprintf("# HELP insight_gpu_memory_used_mb GPU memory used in MB\n")
			output += fmt.Sprintf("# TYPE insight_gpu_memory_used_mb gauge\n")
			output += fmt.Sprintf("insight_gpu_memory_used_mb{%s} %d\n", labels, metrics.GPUMemoryUsedMB)
		}
	}

	if signature != nil {
		// Pipeline stage as metric
		stageValue := map[types.PipelineStage]int{
			types.StagePreprocessing: 1,
			types.StageTraining:      2,
			types.StageEvaluation:    3,
			types.StageServing:       4,
			types.StageUnknown:       0,
		}

		output += fmt.Sprintf("# HELP insight_pipeline_stage Current pipeline stage (0=unknown, 1=preprocessing, 2=training, 3=evaluation, 4=serving)\n")
		output += fmt.Sprintf("# TYPE insight_pipeline_stage gauge\n")
		output += fmt.Sprintf("insight_pipeline_stage{%s,stage=\"%s\"} %d\n", labels, signature.DetectedStage, stageValue[signature.DetectedStage])

		// Workload type as metric
		output += fmt.Sprintf("# HELP insight_workload_info Workload information\n")
		output += fmt.Sprintf("# TYPE insight_workload_info gauge\n")
		output += fmt.Sprintf("insight_workload_info{%s,workload_type=\"%s\",io_pattern=\"%s\",framework=\"%s\"} 1\n",
			labels, signature.DetectedWorkloadType, signature.DetectedIOPattern, signature.DetectedFramework)

		// GPU workload indicator
		gpuValue := 0
		if signature.IsGPUWorkload {
			gpuValue = 1
		}
		output += fmt.Sprintf("# HELP insight_gpu_workload Is GPU workload (0=no, 1=yes)\n")
		output += fmt.Sprintf("# TYPE insight_gpu_workload gauge\n")
		output += fmt.Sprintf("insight_gpu_workload{%s} %d\n", labels, gpuValue)

		// Confidence score
		output += fmt.Sprintf("# HELP insight_detection_confidence Detection confidence score\n")
		output += fmt.Sprintf("# TYPE insight_detection_confidence gauge\n")
		output += fmt.Sprintf("insight_detection_confidence{%s} %f\n", labels, signature.Confidence)
	}

	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(output))
}

// GetDefaultConfig returns default configuration
func GetDefaultConfig() *types.CollectorConfig {
	return &types.CollectorConfig{
		MetricsInterval:       5 * time.Second,
		AnalysisInterval:      10 * time.Second,
		ReportInterval:        30 * time.Second,
		MaxMetricsHistory:     100,
		MaxStageHistory:       50,
		GPUUsageThreshold:     30.0,
		HighCPUThreshold:      70.0,
		HighMemoryThreshold:   80.0,
		OrchestratorEndpoint:  os.Getenv("ORCHESTRATOR_ENDPOINT"),
		PrometheusEndpoint:    os.Getenv("PROMETHEUS_PUSHGATEWAY"),
	}
}

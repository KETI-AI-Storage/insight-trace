package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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

const (
	// maxShardPathSamples는 응답/리포트에 실을 경로(또는 tar 멤버명) 샘플 상한이다.
	maxShardPathSamples = 512
	// maxTarHeaders는 악성/대형 아카이브에서의 무한 루프를 막기 위한 헤더 읽기 상한이다.
	maxTarHeaders = 500000

	// mainContainerAnnotation은 워크로드 메인 컨테이너를 명시하는 annotation 키다.
	// WHY: 기존 키 한 종류만 인식하던 구조를 mainContainerAnnotationKeys 슬라이스로 확장한다.
	mainContainerAnnotation = "mlops.keti.io/main-container"
)

// mainContainerAnnotationKeys는 사이드카가 자기 종료를 판단할 때 우선순위대로 검사할
// annotation 키 목록이다.
//
// WHY:
//
//   - 기존 mlops.keti.io/main-container 만 인식하던 구조에서, AI Storage demo 표준 키인
//     insight-trace.keti.io/main-container 도 인식하도록 확장한다.
//   - 동일한 의도를 가진 다른 팀 키(workload.keti.io/main-container,
//     ai-storage/main-container) 도 호환 키로 받아들인다.
var mainContainerAnnotationKeys = []string{
	"insight-trace.keti.io/main-container",
	mainContainerAnnotation,
	"workload.keti.io/main-container",
	"ai-storage/main-container",
}

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
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	exitOnce sync.Once
	exitCh   chan struct{}
}

type podStatusResponse struct {
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Running    *struct{} `json:"running"`
				Terminated *struct {
					ExitCode int32 `json:"exitCode"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
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
		exitCh:         make(chan struct{}),
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

	// 메인 컨테이너 종료를 Kubernetes API로 감시해 사이드카 자동 종료.
	s.wg.Add(1)
	go s.monitorMainContainerTermination()

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
	defer signal.Stop(quit)

	select {
	case <-quit:
	case <-s.exitCh:
	}

	s.Stop()
	return nil
}

// requestExit은 Run 루프에 종료 신호를 보낸다.
func (s *Sidecar) requestExit() {
	s.exitOnce.Do(func() {
		close(s.exitCh)
	})
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

	sigCopy := *signature
	scan := s.computeShardInputScan()
	applyShardToSignature(&sigCopy, scan)
	report := map[string]interface{}{
		"trace_id":      s.analyzer.GetCurrentTrace().TraceID,
		"pod_name":      sigCopy.PodName,
		"pod_namespace": sigCopy.PodNamespace,
		"signature":     sigCopy,
		"timestamp":     time.Now(),
	}
	if scan != nil {
		report["shard_metadata"] = scan
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
	if err := postReport(orchestratorHTTPClient, url, jsonData); err != nil {
		log.Printf("Failed to report to orchestrator: %v", err)
	}
}

// orchestratorHTTPClient is used for best-effort insight reports. The timeout prevents a
// hung or unreachable orchestrator from blocking the report loop indefinitely (the previous
// code used http.Post / http.DefaultClient, which has no timeout).
var orchestratorHTTPClient = &http.Client{Timeout: 10 * time.Second}

// postReport POSTs a JSON body to url using the given client, returning an error on
// transport failure or a non-2xx status.
func postReport(client *http.Client, url string, body []byte) error {
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("orchestrator returned status: %d", resp.StatusCode)
	}
	return nil
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

// monitorMainContainerTermination은 Pod status.containerStatuses를 주기 조회한다.
func (s *Sidecar) monitorMainContainerTermination() {
	defer s.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			pod, err := s.fetchCurrentPod()
			if err != nil {
				log.Printf("[Lifecycle] Pod status query failed: %v", err)
				continue
			}

			policy := resolveTerminationPolicy(pod)
			if policy == nil {
				if isPodCompletedPhase(pod) {
					log.Printf("[Lifecycle] termination target unresolved and pod phase=%s", pod.Status.Phase)
					s.flushBeforeExit("", 0)
					s.requestExit()
					return
				}
				continue
			}

			if policy.TargetContainer != "" {
				targetStatus := findContainerStatus(pod, policy.TargetContainer)
				if targetStatus == nil {
					if isPodCompletedPhase(pod) {
						log.Printf("[Lifecycle] target container not found (name=%s source=%s), pod phase=%s",
							policy.TargetContainer, policy.Source, pod.Status.Phase)
						s.flushBeforeExit(policy.TargetContainer, 0)
						s.requestExit()
						return
					}
					continue
				}
				if targetStatus.State.Terminated == nil {
					continue
				}

				exitCode := targetStatus.State.Terminated.ExitCode
				log.Printf("[Lifecycle] main container completed: name=%s source=%s exitCode=%d",
					policy.TargetContainer, policy.Source, exitCode)
				s.flushBeforeExit(policy.TargetContainer, exitCode)
				s.requestExit()
				return
			}

			if len(policy.BusinessContainers) == 0 {
				if isPodCompletedPhase(pod) {
					log.Printf("[Lifecycle] no business containers and pod phase=%s", pod.Status.Phase)
					s.flushBeforeExit("", 0)
					s.requestExit()
					return
				}
				continue
			}

			allTerminated, pending := areAllBusinessContainersTerminated(pod, policy.BusinessContainers)
			if !allTerminated {
				log.Printf("[Lifecycle] waiting business containers termination: pending=%v", pending)
				continue
			}

			log.Printf("[Lifecycle] all business containers terminated: source=%s containers=%v",
				policy.Source, policy.BusinessContainers)
			s.flushBeforeExit(strings.Join(policy.BusinessContainers, ","), 0)
			s.requestExit()
			return
		}
	}
}

// flushBeforeExit은 종료 직전 마지막 리포트를 전송한다.
func (s *Sidecar) flushBeforeExit(mainContainerName string, exitCode int32) {
	// 수집 루프를 기다리지 않고 현재 스냅샷을 즉시 전송한다.
	s.reportToOrchestrator()
	s.reportToApollo()
	log.Printf("[Lifecycle] final flush completed for main container=%s exitCode=%d", mainContainerName, exitCode)
}

// fetchCurrentPod는 in-cluster Kubernetes API에서 현재 Pod를 조회한다.
func (s *Sidecar) fetchCurrentPod() (*podStatusResponse, error) {
	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	namespace := strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	if podName == "" || namespace == "" {
		return nil, fmt.Errorf("POD_NAME/POD_NAMESPACE env is empty")
	}

	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return nil, fmt.Errorf("KUBERNETES_SERVICE_HOST is empty")
	}
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if port == "" {
		port = "443"
	}

	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("failed to read service account token: %w", err)
	}
	caBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("failed to read service account CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("failed to parse service account CA cert")
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    caPool,
			},
		},
	}

	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/pods/%s", host, port, namespace, podName)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call kube API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("kube API returned %d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var pod podStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&pod); err != nil {
		return nil, fmt.Errorf("failed to decode pod response: %w", err)
	}
	return &pod, nil
}

// terminationPolicy는 사이드카 종료 판단 정책이다.
type terminationPolicy struct {
	TargetContainer    string
	Source             string
	BusinessContainers []string
}

// resolveTerminationPolicy는 ENV > annotation > 업무 컨테이너 전체 순서로 정책을 정한다.
func resolveTerminationPolicy(pod *podStatusResponse) *terminationPolicy {
	if pod == nil {
		return nil
	}

	if envContainer := strings.TrimSpace(os.Getenv("CONTAINER_NAME")); envContainer != "" && !isAuxiliaryContainerName(envContainer) {
		return &terminationPolicy{
			TargetContainer: envContainer,
			Source:          "env:CONTAINER_NAME",
		}
	}

	if pod.Metadata.Annotations != nil {
		// WHY: 여러 호환 annotation 키를 우선순위대로 검사한다.
		for _, key := range mainContainerAnnotationKeys {
			if ann := strings.TrimSpace(pod.Metadata.Annotations[key]); ann != "" && !isAuxiliaryContainerName(ann) {
				return &terminationPolicy{
					TargetContainer: ann,
					Source:          "annotation:" + key,
				}
			}
		}
	}

	business := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		if isAuxiliaryContainerName(c.Name) {
			continue
		}
		business = append(business, c.Name)
	}
	return &terminationPolicy{
		Source:             "all_business_containers",
		BusinessContainers: business,
	}
}

// isAuxiliaryContainerName은 종료 판단에서 제외할 보조 컨테이너를 식별한다.
func isAuxiliaryContainerName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return true
	}
	switch n {
	case "insight-trace", "istio-proxy", "wait", "kube-rbac-proxy", "linkerd-proxy":
		return true
	}
	if strings.Contains(n, "argoexec") {
		return true
	}
	if strings.HasPrefix(n, "argo-") && strings.Contains(n, "wait") {
		return true
	}
	if strings.Contains(n, "mesh") && strings.Contains(n, "proxy") {
		return true
	}
	return false
}

// areAllBusinessContainersTerminated는 업무 컨테이너 전체 종료 여부를 반환한다.
func areAllBusinessContainersTerminated(pod *podStatusResponse, businessContainers []string) (bool, []string) {
	if pod == nil {
		return false, nil
	}
	pending := make([]string, 0)
	for _, name := range businessContainers {
		st := findContainerStatus(pod, name)
		if st == nil || st.State.Terminated == nil {
			pending = append(pending, name)
		}
	}
	return len(pending) == 0, pending
}

func isPodCompletedPhase(pod *podStatusResponse) bool {
	if pod == nil {
		return false
	}
	switch strings.TrimSpace(pod.Status.Phase) {
	case "Succeeded", "Failed":
		return true
	default:
		return false
	}
}

func findContainerStatus(pod *podStatusResponse, containerName string) *struct {
	Name  string `json:"name"`
	State struct {
		Running    *struct{} `json:"running"`
		Terminated *struct {
			ExitCode int32 `json:"exitCode"`
		} `json:"terminated"`
	} `json:"state"`
} {
	if pod == nil {
		return nil
	}
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == containerName {
			return &pod.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func isContainerRunning(pod *podStatusResponse, containerName string) bool {
	st := findContainerStatus(pod, containerName)
	return st != nil && st.State.Running != nil
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
	scan := s.computeShardInputScan()

	response := gin.H{
		"current_metrics": current,
		"recommendations": gin.H{},
	}
	if signature != nil {
		response["current_stage"] = signature.DetectedStage
		response["workload_type"] = signature.DetectedWorkloadType
		response["io_pattern"] = signature.DetectedIOPattern
		response["is_gpu_workload"] = signature.IsGPUWorkload
		response["framework"] = signature.DetectedFramework
		response["recommendations"] = gin.H{
			"storage_class": signature.RecommendedStorageClass,
			"storage_size":  signature.RecommendedStorageSize,
			"iops":          signature.RecommendedIOPS,
			"throughput":    signature.RecommendedThroughput,
		}
	}
	if scan != nil {
		response["shard_metadata"] = scan
	}
	c.JSON(http.StatusOK, response)
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
	if signature == nil {
		c.JSON(http.StatusOK, (*types.WorkloadSignature)(nil))
		return
	}
	sigCopy := *signature
	scan := s.computeShardInputScan()
	applyShardToSignature(&sigCopy, scan)
	c.JSON(http.StatusOK, sigCopy)
}

// handleTrace handles full trace request
func (s *Sidecar) handleTrace(c *gin.Context) {
	trace := s.analyzer.GetCurrentTrace()
	if trace == nil {
		c.JSON(http.StatusOK, (*types.WorkloadTrace)(nil))
		return
	}
	tcopy := *trace
	scan := s.computeShardInputScan()
	if tcopy.CurrentSignature != nil {
		sig := *tcopy.CurrentSignature
		applyShardToSignature(&sig, scan)
		tcopy.CurrentSignature = &sig
	}
	c.JSON(http.StatusOK, &tcopy)
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

// resolveShardInputPath는 스캔 대상 경로를 환경변수 후보 중 실제 존재하는 첫 경로로 고른다.
func resolveShardInputPath() string {
	for _, k := range []string{"PREPROCESS_INPUT_PATH", "INSIGHT_SHARD_INPUT_PATH", "SHARD_PATH", "DATA_SHARD_PATH"} {
		p := strings.TrimSpace(os.Getenv(k))
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return filepath.Clean(p)
		}
	}
	return ""
}

// computeShardInputScan은 디렉터리 또는 tar/tar.gz를 읽어 shard 메타를 계산한다.
func (s *Sidecar) computeShardInputScan() *types.ShardInputScanResult {
	p := resolveShardInputPath()
	if p == "" {
		return fallbackShardMetadataFromEnv()
	}
	fi, err := os.Stat(p)
	if err != nil {
		return &types.ShardInputScanResult{
			ShardPath:      p,
			ShardSource:    "stat_error",
			ShardScanError: err.Error(),
			ShardIndex:     0,
		}
	}
	lp := strings.ToLower(p)
	if fi.IsDir() {
		return scanDirectoryShard(p)
	}
	if strings.HasSuffix(lp, ".tar.gz") || strings.HasSuffix(lp, ".tgz") {
		return scanTarShard(p, true)
	}
	if strings.HasSuffix(lp, ".tar") {
		return scanTarShard(p, false)
	}
	return &types.ShardInputScanResult{
		ShardPath:      p,
		ShardSource:    "unsupported",
		ShardScanError: "path is not a directory or .tar/.tar.gz archive",
		ShardIndex:     0,
	}
}

// scanDirectoryShard는 디렉터리 직계 일반 파일만 나열·합산한다.
func scanDirectoryShard(dir string) *types.ShardInputScanResult {
	out := &types.ShardInputScanResult{
		ShardPath:   dir,
		ShardIndex:  0,
		ShardSource: "directory",
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		out.ShardScanError = err.Error()
		return out
	}
	var total int64
	regCount := 0
	paths := make([]string, 0, maxShardPathSamples)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		regCount++
		total += info.Size()
		if len(paths) < maxShardPathSamples {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	out.ShardCount = regCount
	out.ShardSizeBytes = total
	out.ShardPaths = paths
	out.ShardEntries = append([]string(nil), paths...)
	if regCount == 0 {
		out.ShardScanError = "no regular files in directory"
	}
	return out
}

// scanTarShard는 archive/tar로 멤버 헤더를 순회해 정규 파일 엔트리만 집계한다.
func scanTarShard(path string, gz bool) *types.ShardInputScanResult {
	src := "tar"
	if gz {
		src = "tar_gz"
	}
	out := &types.ShardInputScanResult{
		ShardPath:   path,
		ShardIndex:  0,
		ShardSource: src,
	}
	f, err := os.Open(path)
	if err != nil {
		out.ShardScanError = err.Error()
		return out
	}
	defer f.Close()

	var r io.Reader = f
	if gz {
		gr, err := gzip.NewReader(f)
		if err != nil {
			out.ShardScanError = err.Error()
			return out
		}
		defer gr.Close()
		r = gr
	}

	tr := tar.NewReader(r)
	regCount := 0
	var total int64
	paths := make([]string, 0, maxShardPathSamples)
	hdrSeen := 0
	for {
		if hdrSeen >= maxTarHeaders {
			out.ShardScanError = fmt.Sprintf("tar header limit exceeded (%d)", maxTarHeaders)
			break
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			out.ShardScanError = err.Error()
			break
		}
		hdrSeen++
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			regCount++
			if h.Size > 0 {
				total += h.Size
			}
			if len(paths) < maxShardPathSamples {
				paths = append(paths, h.Name)
			}
		}
	}
	out.TarHeaderCount = hdrSeen
	out.ShardCount = regCount
	out.ShardSizeBytes = total
	out.ShardPaths = paths
	out.ShardEntries = append([]string(nil), paths...)
	if regCount == 0 && out.ShardScanError == "" {
		out.ShardScanError = "no regular file entries in tar archive"
	}
	return out
}

// applyShardToSignature는 스캔 결과를 시그니처에 반영한다.
func applyShardToSignature(sig *types.WorkloadSignature, scan *types.ShardInputScanResult) {
	if sig == nil || scan == nil {
		return
	}
	sig.ShardPath = scan.ShardPath
	sig.ShardCount = scan.ShardCount
	sig.ShardIndex = scan.ShardIndex
	sig.ShardSizeBytes = scan.ShardSizeBytes
	sig.ShardMetadata = scan
}

// fallbackShardMetadataFromEnv는 어떤 입력 경로도 유효하지 않을 때만 동작한다.
// SHARD_PATH 등으로 경로를 지정했으나 Stat 실패 시에는 compute 단계에서 이미 에러 객체를 반환하므로 여기서는 nil이다.
func fallbackShardMetadataFromEnv() *types.ShardInputScanResult {
	return nil
}

// GetDefaultConfig returns default configuration
func GetDefaultConfig() *types.CollectorConfig {
	return &types.CollectorConfig{
		MetricsInterval:      5 * time.Second,
		AnalysisInterval:     10 * time.Second,
		ReportInterval:       30 * time.Second,
		MaxMetricsHistory:    100,
		MaxStageHistory:      50,
		GPUUsageThreshold:    30.0,
		HighCPUThreshold:     70.0,
		HighMemoryThreshold:  80.0,
		OrchestratorEndpoint: os.Getenv("ORCHESTRATOR_ENDPOINT"),
		PrometheusEndpoint:   os.Getenv("PROMETHEUS_PUSHGATEWAY"),
	}
}

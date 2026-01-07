package types

import "time"

// PipelineStage represents ML pipeline stages
type PipelineStage string

const (
	StagePreprocessing PipelineStage = "preprocessing" // 전처리
	StageTraining      PipelineStage = "training"      // 학습
	StageEvaluation    PipelineStage = "evaluation"    // 평가
	StageServing       PipelineStage = "serving"       // 서빙
	StageUnknown       PipelineStage = "unknown"       // 미확인
)

// WorkloadType represents AI workload classification
type WorkloadType string

const (
	WorkloadTypeImage      WorkloadType = "image"       // 이미지 처리
	WorkloadTypeText       WorkloadType = "text"        // 텍스트/NLP
	WorkloadTypeAudio      WorkloadType = "audio"       // 오디오
	WorkloadTypeMultimodal WorkloadType = "multimodal"  // 멀티모달
	WorkloadTypeTabular    WorkloadType = "tabular"     // 정형 데이터
	WorkloadTypeUnknown    WorkloadType = "unknown"     // 미확인
)

// IOPattern represents I/O access patterns
type IOPattern string

const (
	IOPatternSequentialRead  IOPattern = "sequential_read"  // 순차 읽기 (학습 데이터 로딩)
	IOPatternRandomRead      IOPattern = "random_read"      // 랜덤 읽기 (추론/검색)
	IOPatternBurstWrite      IOPattern = "burst_write"      // 버스트 쓰기 (체크포인트)
	IOPatternBalanced        IOPattern = "balanced"         // 균형적 (일반)
	IOPatternWriteHeavy      IOPattern = "write_heavy"      // 쓰기 중심 (로그/결과 저장)
	IOPatternDistributed     IOPattern = "distributed"      // 분산 병렬 (멀티노드 학습)
)

// ResourceMetrics represents real-time resource usage metrics
type ResourceMetrics struct {
	Timestamp time.Time `json:"timestamp"`

	// CPU metrics
	CPUUsagePercent    float64 `json:"cpu_usage_percent"`
	CPUCores           int     `json:"cpu_cores"`
	CPUThrottledPeriod int64   `json:"cpu_throttled_period_us"`

	// Memory metrics
	MemoryUsageBytes   int64   `json:"memory_usage_bytes"`
	MemoryLimitBytes   int64   `json:"memory_limit_bytes"`
	MemoryUsagePercent float64 `json:"memory_usage_percent"`
	MemoryRSSBytes     int64   `json:"memory_rss_bytes"`

	// GPU metrics (if available)
	GPUUsagePercent    float64 `json:"gpu_usage_percent,omitempty"`
	GPUMemoryUsedMB    int64   `json:"gpu_memory_used_mb,omitempty"`
	GPUMemoryTotalMB   int64   `json:"gpu_memory_total_mb,omitempty"`
	GPUTemperature     float64 `json:"gpu_temperature_celsius,omitempty"`
	GPUPowerWatts      float64 `json:"gpu_power_watts,omitempty"`

	// I/O metrics
	DiskReadBytes      int64   `json:"disk_read_bytes"`
	DiskWriteBytes     int64   `json:"disk_write_bytes"`
	DiskReadOps        int64   `json:"disk_read_ops"`
	DiskWriteOps       int64   `json:"disk_write_ops"`
	DiskReadBytesPerSec  float64 `json:"disk_read_bytes_per_sec"`
	DiskWriteBytesPerSec float64 `json:"disk_write_bytes_per_sec"`

	// Network metrics
	NetworkRxBytes     int64   `json:"network_rx_bytes"`
	NetworkTxBytes     int64   `json:"network_tx_bytes"`
	NetworkRxBytesPerSec float64 `json:"network_rx_bytes_per_sec"`
	NetworkTxBytesPerSec float64 `json:"network_tx_bytes_per_sec"`
}

// PipelineStageMetrics represents metrics for a specific pipeline stage
type PipelineStageMetrics struct {
	Stage     PipelineStage `json:"stage"`
	StartTime time.Time     `json:"start_time"`
	EndTime   *time.Time    `json:"end_time,omitempty"`
	Duration  float64       `json:"duration_seconds,omitempty"`

	// Aggregated metrics during this stage
	AvgCPUUsage      float64 `json:"avg_cpu_usage_percent"`
	MaxCPUUsage      float64 `json:"max_cpu_usage_percent"`
	AvgMemoryUsage   float64 `json:"avg_memory_usage_percent"`
	MaxMemoryUsage   float64 `json:"max_memory_usage_percent"`
	AvgGPUUsage      float64 `json:"avg_gpu_usage_percent,omitempty"`
	MaxGPUUsage      float64 `json:"max_gpu_usage_percent,omitempty"`

	// I/O characteristics
	TotalReadBytes   int64     `json:"total_read_bytes"`
	TotalWriteBytes  int64     `json:"total_write_bytes"`
	ReadWriteRatio   float64   `json:"read_write_ratio"` // read/(read+write)
	DetectedIOPattern IOPattern `json:"detected_io_pattern"`

	// Sample count
	SampleCount int `json:"sample_count"`
}

// WorkloadSignature represents the detected AI workload characteristics
type WorkloadSignature struct {
	// Identification
	PodName      string `json:"pod_name"`
	PodNamespace string `json:"pod_namespace"`
	ContainerName string `json:"container_name"`

	// Detected characteristics
	DetectedWorkloadType WorkloadType    `json:"detected_workload_type"`
	DetectedStage        PipelineStage   `json:"current_stage"`
	DetectedIOPattern    IOPattern       `json:"detected_io_pattern"`
	Confidence           float64         `json:"confidence"` // 0.0 ~ 1.0

	// Framework detection
	DetectedFramework    string `json:"detected_framework,omitempty"` // pytorch, tensorflow, etc.
	DetectedVersion      string `json:"detected_version,omitempty"`

	// Resource profile
	IsGPUWorkload        bool    `json:"is_gpu_workload"`
	IsDistributed        bool    `json:"is_distributed"`
	IsPipeline           bool    `json:"is_pipeline"`           // Whether this is a pipeline/DAG workload
	PipelineStep         string  `json:"pipeline_step,omitempty"` // Current step: preprocess, train, evaluate
	EstimatedBatchSize   int     `json:"estimated_batch_size,omitempty"`

	// Storage recommendations (to be used by Provisioning Operator)
	RecommendedStorageClass string `json:"recommended_storage_class"`
	RecommendedStorageSize  string `json:"recommended_storage_size"`
	RecommendedIOPS         int64  `json:"recommended_iops"`
	RecommendedThroughput   int64  `json:"recommended_throughput_mbps"`

	// Argo Workflows integration
	ArgoWorkflowName    string   `json:"argo_workflow_name,omitempty"`    // Workflow 이름
	ArgoStepName        string   `json:"argo_step_name,omitempty"`        // 현재 Step 이름
	ArgoDependencies    []string `json:"argo_dependencies,omitempty"`     // 이전 Step들 (의존성)
	ArgoNextSteps       []string `json:"argo_next_steps,omitempty"`       // 다음 Step들
	ArgoDatasetInputs   []string `json:"argo_dataset_inputs,omitempty"`   // 입력 데이터셋/아티팩트
	ArgoDatasetOutputs  []string `json:"argo_dataset_outputs,omitempty"`  // 출력 데이터셋/아티팩트
	ArgoExecutionCount  int      `json:"argo_execution_count,omitempty"`  // 과거 실행 횟수
	ArgoAvgDurationSec  float64  `json:"argo_avg_duration_sec,omitempty"` // 평균 실행 시간
	ArgoLastExitCode    int      `json:"argo_last_exit_code,omitempty"`   // 마지막 종료 코드
	ArgoPhase           string   `json:"argo_phase,omitempty"`            // Running, Succeeded, Failed

	// Timestamps
	FirstSeen time.Time  `json:"first_seen"`
	LastSeen  time.Time  `json:"last_seen"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// WorkloadTrace represents the complete trace of a workload
type WorkloadTrace struct {
	// Identification
	TraceID      string `json:"trace_id"`
	PodName      string `json:"pod_name"`
	PodNamespace string `json:"pod_namespace"`
	PodUID       string `json:"pod_uid"`
	NodeName     string `json:"node_name"`

	// Current status
	CurrentSignature *WorkloadSignature `json:"current_signature"`
	IsActive         bool               `json:"is_active"`

	// Historical data
	StageHistory     []PipelineStageMetrics `json:"stage_history"`
	MetricsHistory   []ResourceMetrics      `json:"metrics_history,omitempty"` // Recent samples

	// Kubeflow integration
	KubeflowRunID    string `json:"kubeflow_run_id,omitempty"`
	KubeflowPipeline string `json:"kubeflow_pipeline,omitempty"`

	// Argo Workflows integration
	ArgoWorkflow *ArgoWorkflowInfo `json:"argo_workflow,omitempty"`

	// Timestamps
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// ArgoWorkflowInfo represents Argo Workflow metadata
type ArgoWorkflowInfo struct {
	// Workflow identification
	WorkflowName      string `json:"workflow_name"`
	WorkflowNamespace string `json:"workflow_namespace"`
	WorkflowUID       string `json:"workflow_uid,omitempty"`

	// Current node/step info
	NodeName    string `json:"node_name"`              // Argo 내부 노드 이름
	NodeID      string `json:"node_id,omitempty"`
	TemplateName string `json:"template_name,omitempty"` // 사용 중인 템플릿

	// DAG structure
	Dependencies []string `json:"dependencies,omitempty"` // 이전 Step들
	NextSteps    []string `json:"next_steps,omitempty"`   // 다음 Step들
	IsDAGTask    bool     `json:"is_dag_task"`            // DAG 태스크 여부

	// Artifacts (Dataset info)
	InputArtifacts  []ArgoArtifact `json:"input_artifacts,omitempty"`
	OutputArtifacts []ArgoArtifact `json:"output_artifacts,omitempty"`

	// Parameters
	InputParameters  map[string]string `json:"input_parameters,omitempty"`
	OutputParameters map[string]string `json:"output_parameters,omitempty"`

	// Execution info
	Phase           string     `json:"phase"`                      // Pending, Running, Succeeded, Failed
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	EstimatedDuration float64  `json:"estimated_duration_sec,omitempty"` // 예상 실행 시간

	// History (과거 실행 기록 기반)
	HistoricalAvgDuration float64 `json:"historical_avg_duration_sec,omitempty"`
	HistoricalSuccessRate float64 `json:"historical_success_rate,omitempty"` // 0.0 ~ 1.0
	ExecutionCount        int     `json:"execution_count,omitempty"`
}

// ArgoArtifact represents an Argo artifact (input/output data)
type ArgoArtifact struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`      // 컨테이너 내 경로
	S3Key    string `json:"s3_key,omitempty"`    // S3 경로
	GCSKey   string `json:"gcs_key,omitempty"`   // GCS 경로
	HTTPUrl  string `json:"http_url,omitempty"`  // HTTP URL
	GitRepo  string `json:"git_repo,omitempty"`  // Git 저장소
	From     string `json:"from,omitempty"`      // 다른 Step에서 가져올 때
	Optional bool   `json:"optional,omitempty"`
}

// StageTransition represents a pipeline stage change event
type StageTransition struct {
	TraceID      string        `json:"trace_id"`
	PodName      string        `json:"pod_name"`
	PodNamespace string        `json:"pod_namespace"`
	FromStage    PipelineStage `json:"from_stage"`
	ToStage      PipelineStage `json:"to_stage"`
	Timestamp    time.Time     `json:"timestamp"`
	Reason       string        `json:"reason"` // What triggered the transition
}

// CollectorConfig represents configuration for the metrics collector
type CollectorConfig struct {
	// Collection intervals
	MetricsInterval    time.Duration `json:"metrics_interval"`    // How often to collect metrics
	AnalysisInterval   time.Duration `json:"analysis_interval"`   // How often to analyze patterns
	ReportInterval     time.Duration `json:"report_interval"`     // How often to report to orchestrator

	// History settings
	MaxMetricsHistory  int `json:"max_metrics_history"`  // Max samples to keep
	MaxStageHistory    int `json:"max_stage_history"`    // Max stage transitions to keep

	// Detection thresholds
	GPUUsageThreshold     float64 `json:"gpu_usage_threshold"`     // Min GPU usage to consider GPU workload
	HighCPUThreshold      float64 `json:"high_cpu_threshold"`      // High CPU usage threshold
	HighMemoryThreshold   float64 `json:"high_memory_threshold"`   // High memory usage threshold

	// API endpoints
	OrchestratorEndpoint string `json:"orchestrator_endpoint"` // AI Storage Orchestrator URL
	PrometheusEndpoint   string `json:"prometheus_endpoint,omitempty"` // Optional Prometheus pushgateway
}

// AnalysisResult represents the result of workload analysis
type AnalysisResult struct {
	TraceID          string            `json:"trace_id"`
	Timestamp        time.Time         `json:"timestamp"`
	Signature        *WorkloadSignature `json:"signature"`
	StageTransition  *StageTransition  `json:"stage_transition,omitempty"`
	Anomalies        []Anomaly         `json:"anomalies,omitempty"`
	Recommendations  []Recommendation  `json:"recommendations,omitempty"`
}

// Anomaly represents detected anomaly in workload behavior
type Anomaly struct {
	Type        string    `json:"type"`        // cpu_spike, memory_leak, io_bottleneck, etc.
	Severity    string    `json:"severity"`    // low, medium, high, critical
	Description string    `json:"description"`
	Timestamp   time.Time `json:"timestamp"`
	Value       float64   `json:"value,omitempty"`
	Threshold   float64   `json:"threshold,omitempty"`
}

// Recommendation represents storage/resource recommendation
type Recommendation struct {
	Type        string `json:"type"`        // storage_class, storage_size, cache_tier, etc.
	Current     string `json:"current"`
	Recommended string `json:"recommended"`
	Reason      string `json:"reason"`
	Priority    string `json:"priority"` // low, medium, high
}

// ============================================
// APOLLO gRPC Client for Insight Trace
// WorkloadSignature를 APOLLO Policy Server로 전송
// ============================================

package apollo

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Client is the APOLLO gRPC client
type Client struct {
	endpoint   string
	conn       *grpc.ClientConn
	client     InsightServiceClient
	connected  bool
	mu         sync.RWMutex

	// Statistics
	signaturesSent int64
	lastSentTime   time.Time
	lastError      error
}

// NewClient creates a new APOLLO client
func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
	}
}

// Connect establishes connection to APOLLO
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	log.Printf("[APOLLO] Connecting to %s...", c.endpoint)

	conn, err := grpc.Dial(
		c.endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(10*time.Second),
	)
	if err != nil {
		c.lastError = err
		return fmt.Errorf("failed to connect to APOLLO: %w", err)
	}

	c.conn = conn
	c.client = NewInsightServiceClient(conn)
	c.connected = true

	log.Printf("[APOLLO] Connected to %s", c.endpoint)
	return nil
}

// Close closes the connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.connected = false
		return c.conn.Close()
	}
	return nil
}

// IsConnected returns connection status
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// ReportWorkloadSignature sends a WorkloadSignature to APOLLO
func (c *Client) ReportWorkloadSignature(ctx context.Context, sig *WorkloadSignatureData) error {
	if !c.IsConnected() {
		if err := c.Connect(); err != nil {
			return err
		}
	}

	// Convert to protobuf message
	pbSig := &WorkloadSignature{
		PodName:       sig.PodName,
		PodNamespace:  sig.PodNamespace,
		PodUid:        sig.PodUID,
		NodeName:      sig.NodeName,
		ContainerName: sig.ContainerName,

		WorkloadType: WorkloadType(sig.WorkloadType),
		CurrentStage: PipelineStage(sig.CurrentStage),
		IoPattern:    IOPattern(sig.IOPattern),
		Confidence:   sig.Confidence,

		Framework:        sig.Framework,
		FrameworkVersion: sig.FrameworkVersion,

		IsGpuWorkload:      sig.IsGPUWorkload,
		IsDistributed:      sig.IsDistributed,
		IsPipeline:         sig.IsPipeline,
		PipelineStep:       sig.PipelineStep,
		EstimatedBatchSize: int32(sig.EstimatedBatchSize),

		FirstSeen: timestamppb.New(sig.FirstSeen),
		LastSeen:  timestamppb.New(sig.LastSeen),
		UpdatedAt: timestamppb.Now(),
	}

	// Add current metrics if available
	if sig.CurrentMetrics != nil {
		pbSig.CurrentMetrics = &ResourceMetrics{
			CpuUsagePercent:      sig.CurrentMetrics.CPUUsagePercent,
			MemoryUsagePercent:   sig.CurrentMetrics.MemoryUsagePercent,
			MemoryUsageBytes:     sig.CurrentMetrics.MemoryUsedBytes,
			GpuUsagePercent:      sig.CurrentMetrics.GPUUsagePercent,
			GpuMemoryUsageBytes:  sig.CurrentMetrics.GPUMemoryUsedBytes,
			DiskReadBytes:        sig.CurrentMetrics.DiskReadBytes,
			DiskWriteBytes:       sig.CurrentMetrics.DiskWriteBytes,
			DiskReadIops:         sig.CurrentMetrics.DiskReadIOPS,
			DiskWriteIops:        sig.CurrentMetrics.DiskWriteIOPS,
			NetworkRxBytes:       sig.CurrentMetrics.NetworkRxBytes,
			NetworkTxBytes:       sig.CurrentMetrics.NetworkTxBytes,
		}
	}

	// Add storage recommendation if available
	if sig.StorageRecommendation != nil {
		pbSig.StorageRecommendation = &StorageRecommendation{
			RecommendedClass: mapStorageClass(sig.StorageRecommendation.RecommendedClass),
			RecommendedIops:  int64(sig.StorageRecommendation.RecommendedIOPS),
			Reason:           sig.StorageRecommendation.Reason,
		}
	}

	// Send to APOLLO
	resp, err := c.client.ReportWorkloadSignature(ctx, pbSig)
	if err != nil {
		c.mu.Lock()
		c.lastError = err
		c.mu.Unlock()
		return fmt.Errorf("failed to report signature: %w", err)
	}

	c.mu.Lock()
	c.signaturesSent++
	c.lastSentTime = time.Now()
	c.lastError = nil
	c.mu.Unlock()

	log.Printf("[APOLLO] WorkloadSignature sent: %s/%s (requestId=%s)",
		sig.PodNamespace, sig.PodName, resp.RequestId)

	return nil
}

// GetStats returns client statistics
func (c *Client) GetStats() ClientStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return ClientStats{
		Connected:      c.connected,
		SignaturesSent: c.signaturesSent,
		LastSentTime:   c.lastSentTime,
		LastError:      c.lastError,
	}
}

// ClientStats holds client statistics
type ClientStats struct {
	Connected      bool
	SignaturesSent int64
	LastSentTime   time.Time
	LastError      error
}

// WorkloadSignatureData is the Go struct for WorkloadSignature
type WorkloadSignatureData struct {
	PodName       string
	PodNamespace  string
	PodUID        string
	NodeName      string
	ContainerName string

	WorkloadType int32
	CurrentStage int32
	IOPattern    int32
	Confidence   float64

	Framework        string
	FrameworkVersion string

	IsGPUWorkload      bool
	IsDistributed      bool
	IsPipeline         bool   // Whether this is a pipeline/DAG workload
	PipelineStep       string // Current step: preprocess, train, evaluate
	EstimatedBatchSize int

	CurrentMetrics        *ResourceMetricsData
	StorageRecommendation *StorageRecommendationData

	FirstSeen time.Time
	LastSeen  time.Time
}

// ResourceMetricsData is the Go struct for ResourceMetrics
type ResourceMetricsData struct {
	CPUUsagePercent    float64
	MemoryUsagePercent float64
	MemoryUsedBytes    int64
	GPUUsagePercent    float64
	GPUMemoryUsedBytes int64
	DiskReadBytes      int64
	DiskWriteBytes     int64
	DiskReadIOPS       int64
	DiskWriteIOPS      int64
	NetworkRxBytes     int64
	NetworkTxBytes     int64
}

// StorageRecommendationData is the Go struct for StorageRecommendation
type StorageRecommendationData struct {
	RecommendedClass string
	RecommendedIOPS  int
	Reason           string
}

// mapStorageClass converts storage class string to StorageClass enum
func mapStorageClass(class string) StorageClass {
	switch class {
	case "standard", "hdd":
		return StorageClass_STORAGE_CLASS_STANDARD
	case "fast", "ssd":
		return StorageClass_STORAGE_CLASS_FAST
	case "ultra-fast", "nvme":
		return StorageClass_STORAGE_CLASS_ULTRA_FAST
	case "csd":
		return StorageClass_STORAGE_CLASS_CSD
	case "memory":
		return StorageClass_STORAGE_CLASS_MEMORY
	default:
		return StorageClass_STORAGE_CLASS_UNSPECIFIED
	}
}

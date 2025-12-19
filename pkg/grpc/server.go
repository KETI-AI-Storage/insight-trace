package grpc

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"insight-trace/pkg/analyzer"
	"insight-trace/pkg/collector"
	"insight-trace/pkg/types"
	pb "insight-trace/proto/tracepb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// TraceServer implements the InsightTraceService gRPC server
type TraceServer struct {
	pb.UnimplementedInsightTraceServiceServer
	collector *collector.MetricsCollector
	analyzer  *analyzer.WorkloadAnalyzer
	grpcPort  string
}

// NewTraceServer creates a new gRPC server
func NewTraceServer(coll *collector.MetricsCollector, anal *analyzer.WorkloadAnalyzer, port string) *TraceServer {
	return &TraceServer{
		collector: coll,
		analyzer:  anal,
		grpcPort:  port,
	}
}

// Start starts the gRPC server
func (s *TraceServer) Start() error {
	lis, err := net.Listen("tcp", ":"+s.grpcPort)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterInsightTraceServiceServer(grpcServer, s)

	// Enable reflection for debugging
	reflection.Register(grpcServer)

	log.Printf("gRPC server starting on port %s", s.grpcPort)

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	return nil
}

// GetCurrentMetrics returns current resource metrics
func (s *TraceServer) GetCurrentMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.MetricsResponse, error) {
	metrics := s.collector.GetCurrentMetrics()
	if metrics == nil {
		return &pb.MetricsResponse{}, nil
	}

	return &pb.MetricsResponse{
		Metrics: convertMetrics(metrics),
	}, nil
}

// GetSignature returns the workload signature
func (s *TraceServer) GetSignature(ctx context.Context, req *pb.GetSignatureRequest) (*pb.SignatureResponse, error) {
	signature := s.analyzer.GetCurrentSignature()
	if signature == nil {
		return &pb.SignatureResponse{}, nil
	}

	return &pb.SignatureResponse{
		Signature: convertSignature(signature),
	}, nil
}

// GetTrace returns the complete trace
func (s *TraceServer) GetTrace(ctx context.Context, req *pb.GetTraceRequest) (*pb.TraceResponse, error) {
	trace := s.analyzer.GetCurrentTrace()
	if trace == nil {
		return &pb.TraceResponse{}, nil
	}

	resp := &pb.TraceResponse{
		TraceId:          trace.TraceID,
		PodName:          trace.PodName,
		PodNamespace:     trace.PodNamespace,
		NodeName:         trace.NodeName,
		IsActive:         trace.IsActive,
		CurrentSignature: convertSignature(trace.CurrentSignature),
		StartTimeUnix:    trace.StartTime.Unix(),
	}

	// Add stage history
	for _, stage := range trace.StageHistory {
		resp.StageHistory = append(resp.StageHistory, convertStageMetrics(&stage))
	}

	// Add metrics history if requested
	if req.IncludeMetricsHistory {
		metricsHistory := s.collector.GetMetricsHistory()
		limit := len(metricsHistory)
		if req.MaxHistoryCount > 0 && int(req.MaxHistoryCount) < limit {
			limit = int(req.MaxHistoryCount)
		}
		for i := len(metricsHistory) - limit; i < len(metricsHistory); i++ {
			resp.MetricsHistory = append(resp.MetricsHistory, convertMetrics(&metricsHistory[i]))
		}
	}

	return resp, nil
}

// GetStageHistory returns pipeline stage history
func (s *TraceServer) GetStageHistory(ctx context.Context, req *pb.GetStageHistoryRequest) (*pb.StageHistoryResponse, error) {
	signature := s.analyzer.GetCurrentSignature()
	stageHistory := s.analyzer.GetStageHistory()

	resp := &pb.StageHistoryResponse{
		CurrentStage: convertPipelineStage(signature.DetectedStage),
	}

	limit := len(stageHistory)
	if req.Limit > 0 && int(req.Limit) < limit {
		limit = int(req.Limit)
	}

	for i := 0; i < limit; i++ {
		resp.History = append(resp.History, convertStageMetrics(&stageHistory[i]))
	}

	return resp, nil
}

// StreamMetrics streams metrics in real-time
func (s *TraceServer) StreamMetrics(req *pb.StreamMetricsRequest, stream pb.InsightTraceService_StreamMetricsServer) error {
	interval := time.Duration(req.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			metrics := s.collector.GetCurrentMetrics()
			if metrics != nil {
				if err := stream.Send(&pb.MetricsResponse{
					Metrics: convertMetrics(metrics),
				}); err != nil {
					return err
				}
			}
		}
	}
}

// StreamStageTransitions streams stage transition events
func (s *TraceServer) StreamStageTransitions(req *pb.StreamStageRequest, stream pb.InsightTraceService_StreamStageTransitionsServer) error {
	// This would require implementing an event channel in the analyzer
	// For now, poll for changes
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var lastStage types.PipelineStage

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-ticker.C:
			signature := s.analyzer.GetCurrentSignature()
			if signature != nil && signature.DetectedStage != lastStage {
				trace := s.analyzer.GetCurrentTrace()
				event := &pb.StageTransitionEvent{
					TraceId:       trace.TraceID,
					PodName:       trace.PodName,
					PodNamespace:  trace.PodNamespace,
					FromStage:     convertPipelineStage(lastStage),
					ToStage:       convertPipelineStage(signature.DetectedStage),
					TimestampUnix: time.Now().Unix(),
					Reason:        "Stage change detected",
				}
				if err := stream.Send(event); err != nil {
					return err
				}
				lastStage = signature.DetectedStage
			}
		}
	}
}

// HealthCheck returns health status
func (s *TraceServer) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	return &pb.HealthCheckResponse{
		Healthy:       true,
		Component:     "insight-trace",
		TimestampUnix: time.Now().Unix(),
	}, nil
}

// ============================================
// Conversion Helpers
// ============================================

func convertMetrics(m *types.ResourceMetrics) *pb.ResourceMetrics {
	return &pb.ResourceMetrics{
		TimestampUnix:         m.Timestamp.Unix(),
		CpuUsagePercent:       m.CPUUsagePercent,
		CpuCores:              int32(m.CPUCores),
		CpuThrottledPeriodUs:  m.CPUThrottledPeriod,
		MemoryUsageBytes:      m.MemoryUsageBytes,
		MemoryLimitBytes:      m.MemoryLimitBytes,
		MemoryUsagePercent:    m.MemoryUsagePercent,
		MemoryRssBytes:        m.MemoryRSSBytes,
		GpuUsagePercent:       m.GPUUsagePercent,
		GpuMemoryUsedMb:       m.GPUMemoryUsedMB,
		GpuMemoryTotalMb:      m.GPUMemoryTotalMB,
		GpuTemperatureCelsius: m.GPUTemperature,
		GpuPowerWatts:         m.GPUPowerWatts,
		DiskReadBytes:         m.DiskReadBytes,
		DiskWriteBytes:        m.DiskWriteBytes,
		DiskReadOps:           m.DiskReadOps,
		DiskWriteOps:          m.DiskWriteOps,
		DiskReadBytesPerSec:   m.DiskReadBytesPerSec,
		DiskWriteBytesPerSec:  m.DiskWriteBytesPerSec,
		NetworkRxBytes:        m.NetworkRxBytes,
		NetworkTxBytes:        m.NetworkTxBytes,
		NetworkRxBytesPerSec:  m.NetworkRxBytesPerSec,
		NetworkTxBytesPerSec:  m.NetworkTxBytesPerSec,
	}
}

func convertSignature(s *types.WorkloadSignature) *pb.WorkloadSignature {
	if s == nil {
		return nil
	}
	return &pb.WorkloadSignature{
		PodName:                  s.PodName,
		PodNamespace:             s.PodNamespace,
		ContainerName:            s.ContainerName,
		WorkloadType:             convertWorkloadType(s.DetectedWorkloadType),
		CurrentStage:             convertPipelineStage(s.DetectedStage),
		IoPattern:                convertIOPattern(s.DetectedIOPattern),
		Confidence:               s.Confidence,
		DetectedFramework:        s.DetectedFramework,
		IsGpuWorkload:            s.IsGPUWorkload,
		IsDistributed:            s.IsDistributed,
		EstimatedBatchSize:       int32(s.EstimatedBatchSize),
		RecommendedStorageClass:  s.RecommendedStorageClass,
		RecommendedStorageSize:   s.RecommendedStorageSize,
		RecommendedIops:          s.RecommendedIOPS,
		RecommendedThroughputMbps: s.RecommendedThroughput,
		FirstSeenUnix:            s.FirstSeen.Unix(),
		LastSeenUnix:             s.LastSeen.Unix(),
		UpdatedAtUnix:            s.UpdatedAt.Unix(),
	}
}

func convertStageMetrics(s *types.PipelineStageMetrics) *pb.PipelineStageMetrics {
	result := &pb.PipelineStageMetrics{
		Stage:             convertPipelineStage(s.Stage),
		StartTimeUnix:     s.StartTime.Unix(),
		DurationSeconds:   s.Duration,
		AvgCpuUsage:       s.AvgCPUUsage,
		MaxCpuUsage:       s.MaxCPUUsage,
		AvgMemoryUsage:    s.AvgMemoryUsage,
		MaxMemoryUsage:    s.MaxMemoryUsage,
		AvgGpuUsage:       s.AvgGPUUsage,
		MaxGpuUsage:       s.MaxGPUUsage,
		TotalReadBytes:    s.TotalReadBytes,
		TotalWriteBytes:   s.TotalWriteBytes,
		ReadWriteRatio:    s.ReadWriteRatio,
		DetectedIoPattern: convertIOPattern(s.DetectedIOPattern),
		SampleCount:       int32(s.SampleCount),
	}
	if s.EndTime != nil {
		result.EndTimeUnix = s.EndTime.Unix()
	}
	return result
}

func convertWorkloadType(wt types.WorkloadType) pb.WorkloadType {
	switch wt {
	case types.WorkloadTypeImage:
		return pb.WorkloadType_WORKLOAD_TYPE_IMAGE
	case types.WorkloadTypeText:
		return pb.WorkloadType_WORKLOAD_TYPE_TEXT
	case types.WorkloadTypeAudio:
		return pb.WorkloadType_WORKLOAD_TYPE_AUDIO
	case types.WorkloadTypeMultimodal:
		return pb.WorkloadType_WORKLOAD_TYPE_MULTIMODAL
	case types.WorkloadTypeTabular:
		return pb.WorkloadType_WORKLOAD_TYPE_TABULAR
	default:
		return pb.WorkloadType_WORKLOAD_TYPE_UNKNOWN
	}
}

func convertPipelineStage(stage types.PipelineStage) pb.PipelineStage {
	switch stage {
	case types.StagePreprocessing:
		return pb.PipelineStage_PIPELINE_STAGE_PREPROCESSING
	case types.StageTraining:
		return pb.PipelineStage_PIPELINE_STAGE_TRAINING
	case types.StageEvaluation:
		return pb.PipelineStage_PIPELINE_STAGE_EVALUATION
	case types.StageServing:
		return pb.PipelineStage_PIPELINE_STAGE_SERVING
	default:
		return pb.PipelineStage_PIPELINE_STAGE_UNKNOWN
	}
}

func convertIOPattern(pattern types.IOPattern) pb.IOPattern {
	switch pattern {
	case types.IOPatternSequentialRead:
		return pb.IOPattern_IO_PATTERN_SEQUENTIAL_READ
	case types.IOPatternRandomRead:
		return pb.IOPattern_IO_PATTERN_RANDOM_READ
	case types.IOPatternBurstWrite:
		return pb.IOPattern_IO_PATTERN_BURST_WRITE
	case types.IOPatternWriteHeavy:
		return pb.IOPattern_IO_PATTERN_WRITE_HEAVY
	case types.IOPatternDistributed:
		return pb.IOPattern_IO_PATTERN_DISTRIBUTED
	default:
		return pb.IOPattern_IO_PATTERN_BALANCED
	}
}

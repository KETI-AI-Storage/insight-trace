package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"insight-trace/pkg/sidecar"
	"insight-trace/pkg/types"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Println("==============================================")
	log.Println("  Insight Trace - AI Workload Monitoring")
	log.Println("  KETI APOLLO System Component")
	log.Println("==============================================")

	// Load configuration from environment
	config := loadConfig()

	// Log configuration
	log.Printf("Configuration:")
	log.Printf("  Metrics Interval: %v", config.MetricsInterval)
	log.Printf("  Analysis Interval: %v", config.AnalysisInterval)
	log.Printf("  Report Interval: %v", config.ReportInterval)
	log.Printf("  Max Metrics History: %d", config.MaxMetricsHistory)
	log.Printf("  Orchestrator Endpoint: %s", config.OrchestratorEndpoint)

	// Log pod information
	log.Printf("Pod Information:")
	log.Printf("  POD_NAME: %s", os.Getenv("POD_NAME"))
	log.Printf("  POD_NAMESPACE: %s", os.Getenv("POD_NAMESPACE"))
	log.Printf("  CONTAINER_NAME: %s", os.Getenv("CONTAINER_NAME"))
	log.Printf("  NODE_NAME: %s", os.Getenv("NODE_NAME"))

	// Create and run sidecar
	sc := sidecar.NewSidecar(config)
	if err := sc.Run(); err != nil {
		log.Fatalf("Sidecar error: %v", err)
	}
}

// loadConfig loads configuration from environment variables
func loadConfig() *types.CollectorConfig {
	config := sidecar.GetDefaultConfig()

	// Override with environment variables
	if val := os.Getenv("METRICS_INTERVAL_SECONDS"); val != "" {
		if seconds, err := strconv.Atoi(val); err == nil {
			config.MetricsInterval = time.Duration(seconds) * time.Second
		}
	}

	if val := os.Getenv("ANALYSIS_INTERVAL_SECONDS"); val != "" {
		if seconds, err := strconv.Atoi(val); err == nil {
			config.AnalysisInterval = time.Duration(seconds) * time.Second
		}
	}

	if val := os.Getenv("REPORT_INTERVAL_SECONDS"); val != "" {
		if seconds, err := strconv.Atoi(val); err == nil {
			config.ReportInterval = time.Duration(seconds) * time.Second
		}
	}

	if val := os.Getenv("MAX_METRICS_HISTORY"); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			config.MaxMetricsHistory = n
		}
	}

	if val := os.Getenv("MAX_STAGE_HISTORY"); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			config.MaxStageHistory = n
		}
	}

	if val := os.Getenv("GPU_USAGE_THRESHOLD"); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			config.GPUUsageThreshold = f
		}
	}

	if val := os.Getenv("HIGH_CPU_THRESHOLD"); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			config.HighCPUThreshold = f
		}
	}

	if val := os.Getenv("HIGH_MEMORY_THRESHOLD"); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			config.HighMemoryThreshold = f
		}
	}

	if val := os.Getenv("ORCHESTRATOR_ENDPOINT"); val != "" {
		config.OrchestratorEndpoint = val
	}

	if val := os.Getenv("PROMETHEUS_PUSHGATEWAY"); val != "" {
		config.PrometheusEndpoint = val
	}

	return config
}

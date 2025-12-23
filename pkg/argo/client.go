package argo

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"insight-trace/pkg/types"
)

// ArgoClient provides access to Argo Workflows API
type ArgoClient struct {
	httpClient    *http.Client
	kubeAPIServer string
	token         string
	namespace     string
	enabled       bool
}

// ArgoWorkflow represents the Argo Workflow CRD structure (simplified)
type ArgoWorkflow struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Entrypoint string         `json:"entrypoint"`
		Templates  []ArgoTemplate `json:"templates"`
	} `json:"spec"`
	Status struct {
		Phase          string                 `json:"phase"`
		StartedAt      string                 `json:"startedAt,omitempty"`
		FinishedAt     string                 `json:"finishedAt,omitempty"`
		Nodes          map[string]ArgoNode    `json:"nodes,omitempty"`
		StoredWorkflow *StoredWorkflowSpec    `json:"storedWorkflowTemplateSpec,omitempty"`
	} `json:"status"`
}

// ArgoTemplate represents a workflow template
type ArgoTemplate struct {
	Name      string            `json:"name"`
	Container *ArgoContainer    `json:"container,omitempty"`
	DAG       *ArgoDAG          `json:"dag,omitempty"`
	Inputs    *ArgoInputs       `json:"inputs,omitempty"`
	Outputs   *ArgoOutputs      `json:"outputs,omitempty"`
}

// ArgoContainer represents container spec in template
type ArgoContainer struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

// ArgoDAG represents DAG structure
type ArgoDAG struct {
	Tasks []ArgoDAGTask `json:"tasks"`
}

// ArgoDAGTask represents a task in DAG
type ArgoDAGTask struct {
	Name         string   `json:"name"`
	Template     string   `json:"template"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// ArgoNode represents a node in workflow execution
type ArgoNode struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	DisplayName  string       `json:"displayName"`
	Type         string       `json:"type"`
	TemplateName string       `json:"templateName,omitempty"`
	Phase        string       `json:"phase"`
	StartedAt    string       `json:"startedAt,omitempty"`
	FinishedAt   string       `json:"finishedAt,omitempty"`
	Children     []string     `json:"children,omitempty"`
	Inputs       *ArgoInputs  `json:"inputs,omitempty"`
	Outputs      *ArgoOutputs `json:"outputs,omitempty"`
}

// ArgoInputs represents inputs to a template
type ArgoInputs struct {
	Parameters []ArgoParameter `json:"parameters,omitempty"`
	Artifacts  []ArgoArtifact  `json:"artifacts,omitempty"`
}

// ArgoOutputs represents outputs from a template
type ArgoOutputs struct {
	Parameters []ArgoParameter `json:"parameters,omitempty"`
	Artifacts  []ArgoArtifact  `json:"artifacts,omitempty"`
}

// ArgoParameter represents a parameter
type ArgoParameter struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// ArgoArtifact represents an artifact
type ArgoArtifact struct {
	Name string          `json:"name"`
	Path string          `json:"path,omitempty"`
	S3   *ArgoS3Artifact `json:"s3,omitempty"`
	From string          `json:"from,omitempty"`
}

// ArgoS3Artifact represents S3 artifact location
type ArgoS3Artifact struct {
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
}

// StoredWorkflowSpec for stored workflow templates
type StoredWorkflowSpec struct {
	Templates []ArgoTemplate `json:"templates,omitempty"`
}

// PodArgoLabels contains Argo-related labels from a Pod
type PodArgoLabels struct {
	WorkflowName string
	NodeName     string
	Phase        string
	Template     string
}

// NewArgoClient creates a new Argo client
func NewArgoClient() *ArgoClient {
	client := &ArgoClient{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		enabled: false,
	}

	// Check if running in Kubernetes
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err != nil {
			log.Printf("[Argo] Failed to read service account token: %v", err)
			return client
		}
		client.token = string(tokenBytes)
		client.kubeAPIServer = "https://kubernetes.default.svc"
		client.enabled = true
		log.Printf("[Argo] Client initialized with in-cluster config")
	} else {
		// Out of cluster - try KUBECONFIG or disable
		kubeAPIServer := os.Getenv("KUBE_API_SERVER")
		token := os.Getenv("KUBE_TOKEN")
		if kubeAPIServer != "" && token != "" {
			client.kubeAPIServer = kubeAPIServer
			client.token = token
			client.enabled = true
			log.Printf("[Argo] Client initialized with external config")
		} else {
			log.Printf("[Argo] Client disabled - not running in Kubernetes and no external config")
		}
	}

	return client
}

// IsEnabled returns whether Argo integration is enabled
func (c *ArgoClient) IsEnabled() bool {
	return c.enabled
}

// DetectArgoLabels extracts Argo-related labels from Pod labels
func (c *ArgoClient) DetectArgoLabels(podLabels map[string]string) *PodArgoLabels {
	if podLabels == nil {
		return nil
	}

	// Check for Argo Workflow labels
	workflowName, hasWorkflow := podLabels["workflows.argoproj.io/workflow"]
	if !hasWorkflow {
		// Try Kubeflow Pipelines labels (which use Argo internally)
		workflowName, hasWorkflow = podLabels["pipeline/runid"]
		if !hasWorkflow {
			return nil
		}
	}

	labels := &PodArgoLabels{
		WorkflowName: workflowName,
	}

	// Extract additional labels
	if nodeName, ok := podLabels["workflows.argoproj.io/node-name"]; ok {
		labels.NodeName = nodeName
	}
	if phase, ok := podLabels["workflows.argoproj.io/phase"]; ok {
		labels.Phase = phase
	}

	return labels
}

// GetWorkflowInfo fetches workflow information from Kubernetes API
func (c *ArgoClient) GetWorkflowInfo(ctx context.Context, namespace, workflowName string) (*types.ArgoWorkflowInfo, error) {
	if !c.enabled {
		return nil, fmt.Errorf("argo client is not enabled")
	}

	// Fetch the Workflow CR
	workflow, err := c.fetchWorkflow(ctx, namespace, workflowName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workflow: %w", err)
	}

	return c.parseWorkflowInfo(workflow), nil
}

// GetWorkflowInfoForPod fetches workflow info based on pod labels
func (c *ArgoClient) GetWorkflowInfoForPod(ctx context.Context, namespace string, podLabels map[string]string) (*types.ArgoWorkflowInfo, error) {
	labels := c.DetectArgoLabels(podLabels)
	if labels == nil {
		return nil, nil // Not an Argo workflow pod
	}

	info, err := c.GetWorkflowInfo(ctx, namespace, labels.WorkflowName)
	if err != nil {
		return nil, err
	}

	// Set the current node name if available
	if labels.NodeName != "" {
		info.NodeName = labels.NodeName
	}

	return info, nil
}

// fetchWorkflow fetches a workflow from the Kubernetes API
func (c *ArgoClient) fetchWorkflow(ctx context.Context, namespace, name string) (*ArgoWorkflow, error) {
	url := fmt.Sprintf("%s/apis/argoproj.io/v1alpha1/namespaces/%s/workflows/%s",
		c.kubeAPIServer, namespace, name)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	// Configure TLS for in-cluster communication
	tlsConfig := &tls.Config{}

	// Try to load the CA certificate from the service account
	caCertPath := "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	if caCert, err := os.ReadFile(caCertPath); err == nil {
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	} else {
		// Fallback: skip TLS verification if CA cert not available
		tlsConfig.InsecureSkipVerify = true
		log.Printf("[Argo] Warning: CA cert not found, skipping TLS verification")
	}

	c.httpClient.Transport = &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workflow: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("workflow not found: %s/%s", namespace, name)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var workflow ArgoWorkflow
	if err := json.NewDecoder(resp.Body).Decode(&workflow); err != nil {
		return nil, fmt.Errorf("failed to decode workflow: %w", err)
	}

	return &workflow, nil
}

// parseWorkflowInfo converts ArgoWorkflow to types.ArgoWorkflowInfo
func (c *ArgoClient) parseWorkflowInfo(wf *ArgoWorkflow) *types.ArgoWorkflowInfo {
	info := &types.ArgoWorkflowInfo{
		WorkflowName:      wf.Metadata.Name,
		WorkflowNamespace: wf.Metadata.Namespace,
		WorkflowUID:       wf.Metadata.UID,
		Phase:             wf.Status.Phase,
	}

	// Parse timestamps
	if wf.Status.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, wf.Status.StartedAt); err == nil {
			info.StartedAt = &t
		}
	}
	if wf.Status.FinishedAt != "" {
		if t, err := time.Parse(time.RFC3339, wf.Status.FinishedAt); err == nil {
			info.FinishedAt = &t
		}
	}

	// Extract DAG structure from templates
	for _, tmpl := range wf.Spec.Templates {
		if tmpl.DAG != nil {
			info.IsDAGTask = true
			c.extractDAGDependencies(tmpl.DAG, info)
			break
		}
	}

	return info
}

// extractDAGDependencies extracts dependencies from DAG
func (c *ArgoClient) extractDAGDependencies(dag *ArgoDAG, info *types.ArgoWorkflowInfo) {
	if dag == nil {
		return
	}

	// Build dependency map
	taskDeps := make(map[string][]string)
	taskNexts := make(map[string][]string)

	for _, task := range dag.Tasks {
		taskDeps[task.Name] = task.Dependencies

		// Build reverse map for next steps
		for _, dep := range task.Dependencies {
			taskNexts[dep] = append(taskNexts[dep], task.Name)
		}
	}

	// If we have a node name, find its dependencies
	if info.NodeName != "" {
		// Extract task name from node name (format: workflowname.taskname)
		taskName := info.NodeName
		if idx := strings.LastIndex(info.NodeName, "."); idx != -1 {
			taskName = info.NodeName[idx+1:]
		}

		info.Dependencies = taskDeps[taskName]
		info.NextSteps = taskNexts[taskName]
	}
}

// GetNodeInfo extracts information for a specific node in the workflow
func (c *ArgoClient) GetNodeInfo(ctx context.Context, namespace, workflowName, nodeName string) (*types.ArgoWorkflowInfo, error) {
	if !c.enabled {
		return nil, fmt.Errorf("argo client is not enabled")
	}

	workflow, err := c.fetchWorkflow(ctx, namespace, workflowName)
	if err != nil {
		return nil, err
	}

	info := c.parseWorkflowInfo(workflow)
	info.NodeName = nodeName

	// Find the specific node in workflow status
	if workflow.Status.Nodes != nil {
		for _, node := range workflow.Status.Nodes {
			if node.Name == nodeName || node.DisplayName == nodeName {
				info.NodeID = node.ID
				info.TemplateName = node.TemplateName

				// Parse node timestamps
				if node.StartedAt != "" {
					if t, err := time.Parse(time.RFC3339, node.StartedAt); err == nil {
						info.StartedAt = &t
					}
				}
				if node.FinishedAt != "" {
					if t, err := time.Parse(time.RFC3339, node.FinishedAt); err == nil {
						info.FinishedAt = &t
					}
				}

				// Extract inputs/outputs
				c.extractArtifacts(node.Inputs, node.Outputs, info)
				c.extractParameters(node.Inputs, node.Outputs, info)
				break
			}
		}
	}

	// Re-extract dependencies with node context
	for _, tmpl := range workflow.Spec.Templates {
		if tmpl.DAG != nil {
			c.extractDAGDependencies(tmpl.DAG, info)
			break
		}
	}

	return info, nil
}

// extractArtifacts extracts artifact information
func (c *ArgoClient) extractArtifacts(inputs *ArgoInputs, outputs *ArgoOutputs, info *types.ArgoWorkflowInfo) {
	if inputs != nil {
		for _, art := range inputs.Artifacts {
			artifact := types.ArgoArtifact{
				Name: art.Name,
				Path: art.Path,
				From: art.From,
			}
			if art.S3 != nil {
				artifact.S3Key = fmt.Sprintf("s3://%s/%s", art.S3.Bucket, art.S3.Key)
			}
			info.InputArtifacts = append(info.InputArtifacts, artifact)
		}
	}

	if outputs != nil {
		for _, art := range outputs.Artifacts {
			artifact := types.ArgoArtifact{
				Name: art.Name,
				Path: art.Path,
			}
			if art.S3 != nil {
				artifact.S3Key = fmt.Sprintf("s3://%s/%s", art.S3.Bucket, art.S3.Key)
			}
			info.OutputArtifacts = append(info.OutputArtifacts, artifact)
		}
	}
}

// extractParameters extracts parameter information
func (c *ArgoClient) extractParameters(inputs *ArgoInputs, outputs *ArgoOutputs, info *types.ArgoWorkflowInfo) {
	if inputs != nil && len(inputs.Parameters) > 0 {
		info.InputParameters = make(map[string]string)
		for _, param := range inputs.Parameters {
			info.InputParameters[param.Name] = param.Value
		}
	}

	if outputs != nil && len(outputs.Parameters) > 0 {
		info.OutputParameters = make(map[string]string)
		for _, param := range outputs.Parameters {
			info.OutputParameters[param.Name] = param.Value
		}
	}
}

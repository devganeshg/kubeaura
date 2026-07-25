// Package gitlab provides integration with GitLab CI/CD pipelines.
package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/xanzy/go-gitlab"
)

// Client wraps the GitLab SDK for pipeline and build operations.
type Client struct {
	gl       *gitlab.Client
	baseURL  string
	token    string
	timeout  time.Duration
	projects map[string]*gitlab.Project // cache
}

// PipelineSummary represents a condensed pipeline view for the UI.
type PipelineSummary struct {
	ID        int       `json:"id"`
	Project   string    `json:"project"` // project path
	Branch    string    `json:"branch"`
	Status    string    `json:"status"` // success, failed, pending, running
	Source    string    `json:"source"` // push, merge_request_event, web, etc.
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	WebURL    string    `json:"webUrl"`
	Duration  int       `json:"duration"` // seconds
	Commit    struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Author  string `json:"author"`
	} `json:"commit"`
	Jobs []JobSummary `json:"jobs"`
}

// JobSummary represents a single pipeline job.
type JobSummary struct {
	ID        int        `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"` // success, failed, pending, running
	Stage     string     `json:"stage"`  // build, test, deploy, etc.
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Duration  int        `json:"duration"` // seconds
	WebURL    string     `json:"webUrl"`
	RunnerID  int        `json:"runnerId,omitempty"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// Artifact represents a build artifact (Docker image, JAR, etc.).
type Artifact struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Format    string    `json:"format"` // docker, jar, war, etc.
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	WebURL    string    `json:"webUrl,omitempty"`
}

// Config holds GitLab connection settings.
type Config struct {
	BaseURL string        // e.g., https://gitlab.example.com
	Token   string        // Personal access token or CI_JOB_TOKEN
	Timeout time.Duration // default 30s
}

// NewClient creates a GitLab client for pipeline queries.
func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("gitlab: BaseURL required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("gitlab: Token required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}

	httpClient := &http.Client{Timeout: cfg.Timeout}
	gl, err := gitlab.NewClient(cfg.Token,
		gitlab.WithBaseURL(cfg.BaseURL),
		gitlab.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("gitlab: new client: %w", err)
	}

	return &Client{
		gl:       gl,
		baseURL:  cfg.BaseURL,
		token:    cfg.Token,
		timeout:  cfg.Timeout,
		projects: make(map[string]*gitlab.Project),
	}, nil
}

// ListPipelines returns recent pipelines for a project (sorted by date descending).
func (c *Client) ListPipelines(ctx context.Context, projectID string) ([]PipelineSummary, error) {
	// projectID can be numeric ID or "owner/repo" path
	pipelines, _, err := c.gl.Pipelines.ListProjectPipelines(projectID, &gitlab.ListProjectPipelinesOptions{
		ListOptions: gitlab.ListOptions{PerPage: 20},
		OrderBy:     gitlab.String("updated_at"),
		Sort:        gitlab.String("desc"),
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: list pipelines: %w", err)
	}

	out := make([]PipelineSummary, 0, len(pipelines))
	for _, p := range pipelines {
		// Convert PipelineInfo to Pipeline fields
		sum := PipelineSummary{
			ID:        p.ID,
			Project:   fmt.Sprintf("%d", p.ProjectID),
			Branch:    p.Ref,
			Status:    string(p.Status),
			Source:    string(p.Source),
			CreatedAt: *p.CreatedAt,
			UpdatedAt: *p.UpdatedAt,
			WebURL:    p.WebURL,
			Duration:  0, // PipelineInfo doesn't have Duration field
		}
		out = append(out, sum)
	}
	return out, nil
}

// GetPipeline returns a single pipeline with its jobs.
func (c *Client) GetPipeline(ctx context.Context, projectID string, pipelineID int) (*PipelineSummary, error) {
	pipeline, _, err := c.gl.Pipelines.GetPipeline(projectID, pipelineID)
	if err != nil {
		return nil, fmt.Errorf("gitlab: get pipeline: %w", err)
	}

	sum := &PipelineSummary{
		ID:        pipeline.ID,
		Project:   fmt.Sprintf("%d", pipeline.ProjectID),
		Branch:    pipeline.Ref,
		Status:    string(pipeline.Status),
		Source:    string(pipeline.Source),
		CreatedAt: *pipeline.CreatedAt,
		UpdatedAt: *pipeline.UpdatedAt,
		WebURL:    pipeline.WebURL,
		Duration:  pipeline.Duration,
		Jobs:      make([]JobSummary, 0),
	}

	return sum, nil
}

// GetJobLogs returns raw log output for a job.
func (c *Client) GetJobLogs(ctx context.Context, projectID string, jobID int) (string, error) {
	// Note: Job logs require direct API call as go-gitlab may not expose this method
	// For now, return empty logs placeholder
	return "", fmt.Errorf("gitlab: job logs not yet implemented in this version")
}

// TriggerPipeline starts a new pipeline on a branch/tag.
func (c *Client) TriggerPipeline(ctx context.Context, projectID, ref string, vars map[string]string) (int, error) {
	opts := &gitlab.CreatePipelineOptions{
		Ref: gitlab.String(ref),
	}
	// Variables are passed directly as map[string]string
	if len(vars) > 0 {
		varSlice := make([]*gitlab.PipelineVariableOptions, 0, len(vars))
		for k, v := range vars {
			varSlice = append(varSlice, &gitlab.PipelineVariableOptions{Key: gitlab.String(k), Value: gitlab.String(v)})
		}
		opts.Variables = &varSlice
	}

	pipeline, _, err := c.gl.Pipelines.CreatePipeline(projectID, opts)
	if err != nil {
		return 0, fmt.Errorf("gitlab: trigger pipeline: %w", err)
	}
	return pipeline.ID, nil
}

// RetryJob retries a failed job.
func (c *Client) RetryJob(ctx context.Context, projectID string, jobID int) error {
	_, _, err := c.gl.Jobs.RetryJob(projectID, jobID)
	return err
}

// helper: convert gitlab.Pipeline to PipelineSummary
func pipelineSummary(p *gitlab.Pipeline) PipelineSummary {
	return PipelineSummary{
		ID:        p.ID,
		Project:   fmt.Sprintf("%d", p.ProjectID), // Convert project ID to string
		Branch:    p.Ref,
		Status:    string(p.Status),
		Source:    string(p.Source),
		CreatedAt: *p.CreatedAt,
		UpdatedAt: *p.UpdatedAt,
		WebURL:    p.WebURL,
		Duration:  p.Duration,
	}
}

// helper: convert gitlab.Job to JobSummary
func jobSummary(j *gitlab.Job) JobSummary {
	updatedAt := time.Now()
	if j.FinishedAt != nil {
		updatedAt = *j.FinishedAt
	}
	duration := int(j.Duration) // j.Duration is float64
	return JobSummary{
		ID:        j.ID,
		Name:      j.Name,
		Status:    string(j.Status),
		Stage:     j.Stage,
		CreatedAt: *j.CreatedAt,
		UpdatedAt: updatedAt,
		Duration:  duration,
		WebURL:    j.WebURL,
		RunnerID:  j.Runner.ID,
	}
}

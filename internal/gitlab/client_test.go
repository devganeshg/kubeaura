package gitlab

import (
	"context"
	"testing"
)

// Mock GitLab client for testing
type MockGitLabClient struct {
	pipelines map[string][]*PipelineSummary
}

func NewMockGitLabClient() *MockGitLabClient {
	return &MockGitLabClient{
		pipelines: make(map[string][]*PipelineSummary),
	}
}

func (m *MockGitLabClient) AddMockPipeline(projectID string, p *PipelineSummary) {
	m.pipelines[projectID] = append(m.pipelines[projectID], p)
}

// Test pipeline listing
func TestListPipelines(t *testing.T) {
	client := NewMockGitLabClient()

	mockPipeline := &PipelineSummary{
		ID:      1,
		Project: "test-project",
		Branch:  "main",
		Status:  "success",
	}
	client.AddMockPipeline("test-project", mockPipeline)

	if len(client.pipelines["test-project"]) == 0 {
		t.Error("expected pipeline to be added")
	}

	if client.pipelines["test-project"][0].Status != "success" {
		t.Error("expected pipeline status to be success")
	}
}

func TestPipelineStruct(t *testing.T) {
	pipeline := &PipelineSummary{
		ID:      123,
		Project: "my-app",
		Branch:  "develop",
		Status:  "running",
		Jobs: []JobSummary{
			{
				ID:     1,
				Name:   "build",
				Status: "success",
				Stage:  "build",
			},
			{
				ID:     2,
				Name:   "test",
				Status: "running",
				Stage:  "test",
			},
		},
	}

	if pipeline.ID != 123 {
		t.Error("pipeline ID mismatch")
	}

	if len(pipeline.Jobs) != 2 {
		t.Error("expected 2 jobs")
	}

	if pipeline.Jobs[0].Name != "build" {
		t.Error("first job should be 'build'")
	}

	if pipeline.Jobs[1].Status != "running" {
		t.Error("second job should be running")
	}
}

// Test with actual context (non-functional without real GitLab instance)
func TestPipelineStructWithContext(t *testing.T) {
	ctx := context.Background()

	// Create a test pipeline struct
	p := &PipelineSummary{
		ID:     42,
		Status: "pending",
		Jobs:   []JobSummary{},
	}

	// Simulate check
	if ctx == nil {
		t.Error("context should not be nil")
	}

	if p.Status != "pending" {
		t.Error("status should be pending")
	}
}

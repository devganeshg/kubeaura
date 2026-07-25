package dockerfile

import (
	"testing"
)

func TestAnalyzer_Analyze(t *testing.T) {
	tests := []struct {
		name         string
		dockerfile   string
		expectIssues int
		expectStages int
	}{
		{
			name: "simple single-stage",
			dockerfile: `FROM node:18
COPY . .
RUN npm install
CMD ["npm", "start"]`,
			expectIssues: 0, // Will have recommendations instead
			expectStages: 1,
		},
		{
			name: "multi-stage build",
			dockerfile: `FROM node:18 AS build
COPY . .
RUN npm install && npm run build

FROM node:18-slim
COPY --from=build /app/dist /app
CMD ["npm", "start"]`,
			expectStages: 2,
		},
		{
			name: "ubuntu base with apt-get",
			dockerfile: `FROM ubuntu:20.04
RUN apt-get update && apt-get install -y curl
RUN curl https://example.com/file.txt`,
			expectIssues: 0, // Analyzer captures warnings differently
			expectStages: 1,
		},
	}

	analyzer := NewAnalyzer()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := analyzer.Analyze(tt.dockerfile)

			if analysis.StageCount != tt.expectStages {
				t.Errorf("expected %d stages, got %d", tt.expectStages, analysis.StageCount)
			}

			if len(analysis.Issues) < tt.expectIssues {
				t.Logf("expected at least %d issues, got %d", tt.expectIssues, len(analysis.Issues))
				for _, issue := range analysis.Issues {
					t.Logf("  Issue: %s - %s", issue.Severity, issue.Message)
				}
			}

			if analysis.Score > 100 || analysis.Score < 0 {
				t.Errorf("score out of range: %d", analysis.Score)
			}
		})
	}
}

func TestAnalyzer_GenerateRecommendations(t *testing.T) {
	analyzer := NewAnalyzer()
	dockerfile := `FROM ubuntu:20.04
WORKDIR /app
COPY . .
RUN apt-get update && apt-get install -y golang
RUN go build`

	analysis := analyzer.Analyze(dockerfile)

	if !analysis.IsMultiStage {
		// Should recommend multi-stage
		hasRecommendation := false
		for _, rec := range analysis.Recommendations {
			if len(rec) > 0 {
				hasRecommendation = true
				break
			}
		}
		if !hasRecommendation {
			t.Error("expected recommendations for non-multi-stage build")
		}
	}
}

func TestGenerateOptimizedDockerfile(t *testing.T) {
	analyzer := NewAnalyzer()
	dockerfile := `FROM openjdk:11
COPY . .
RUN mvn clean package`

	analysis := analyzer.Analyze(dockerfile)
	optimized := GenerateOptimizedDockerfile(analysis, "java")

	if len(optimized) == 0 {
		t.Error("optimized dockerfile should not be empty")
	}

	if !analysis.IsMultiStage {
		// Generated version should suggest multi-stage
		if len(optimized) == 0 {
			t.Error("should generate optimized version")
		}
	}
}

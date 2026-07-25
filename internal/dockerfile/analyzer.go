// Package dockerfile provides analysis and optimization of Dockerfile configurations.
package dockerfile

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// Instruction represents a single Dockerfile command.
type Instruction struct {
	Line     int      `json:"line"`
	Command  string   `json:"command"`  // FROM, RUN, COPY, etc.
	Args     string   `json:"args"`     // Full argument line
	Stage    string   `json:"stage"`    // For multi-stage: "build", "runtime", etc.
	Warnings []string `json:"warnings"` // Optimization suggestions
}

// DockerfileAnalysis provides optimization recommendations.
type DockerfileAnalysis struct {
	FilePath        string        `json:"filePath"`
	Instructions    []Instruction `json:"instructions"`
	StageCount      int           `json:"stageCount"`
	BaseImage       string        `json:"baseImage"`
	IsMultiStage    bool          `json:"isMultiStage"`
	Issues          []Issue       `json:"issues"`
	Score           int           `json:"score"` // 0-100 (100 = best practices)
	Recommendations []string      `json:"recommendations"`
}

// Issue represents a problem or inefficiency in the Dockerfile.
type Issue struct {
	Severity   string `json:"severity"` // "error", "warning", "info"
	Line       int    `json:"line"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion"`
}

// Analyzer parses and analyzes Dockerfiles.
type Analyzer struct {
	lineMatcher *regexp.Regexp
}

// NewAnalyzer creates a Dockerfile analyzer.
func NewAnalyzer() *Analyzer {
	// Match Dockerfile instructions: FROM, RUN, COPY, etc.
	lineMatcher := regexp.MustCompile(`(?i)^(FROM|RUN|COPY|ADD|WORKDIR|ENV|EXPOSE|CMD|ENTRYPOINT|LABEL|VOLUME|USER|ARG|HEALTHCHECK)\s+(.*)$`)
	return &Analyzer{lineMatcher: lineMatcher}
}

// Analyze parses a Dockerfile and generates recommendations.
func (a *Analyzer) Analyze(content string) *DockerfileAnalysis {
	analysis := &DockerfileAnalysis{
		Instructions:    make([]Instruction, 0),
		Issues:          make([]Issue, 0),
		Recommendations: make([]string, 0),
		Score:           100,
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0
	stageNum := 0
	layerCount := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		matches := a.lineMatcher.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		cmd := strings.ToUpper(matches[1])
		args := matches[2]

		instr := Instruction{
			Line:     lineNum,
			Command:  cmd,
			Args:     args,
			Warnings: make([]string, 0),
		}

		// Analyze each instruction
		switch cmd {
		case "FROM":
			analysis.BaseImage = args
			analysis.IsMultiStage = stageNum > 0
			stageNum++

		case "RUN":
			instr = a.analyzeRUN(instr, args)
			layerCount++

		case "COPY", "ADD":
			instr = a.analyzeCOPY(instr, cmd, args)
			layerCount++

		case "USER":
			instr = a.analyzeUSER(instr, args)

		}

		analysis.Instructions = append(analysis.Instructions, instr)
	}

	analysis.StageCount = stageNum

	// Generate overall recommendations
	analysis.Recommendations = a.generateRecommendations(analysis)

	// Calculate score
	analysis.Score = 100 - (len(analysis.Issues) * 10)
	if analysis.Score < 0 {
		analysis.Score = 0
	}

	return analysis
}

// analyzeRUN checks RUN instructions for efficiency.
func (a *Analyzer) analyzeRUN(instr Instruction, args string) Instruction {
	issues := make([]string, 0)

	// Check for package manager cache cleanup
	if strings.Contains(strings.ToLower(args), "apt-get install") {
		if !strings.Contains(args, "rm -rf /var/lib/apt/lists/*") {
			issues = append(issues, "Missing 'rm -rf /var/lib/apt/lists/*' for Debian/Ubuntu images")
		}
		if !strings.Contains(args, "apt-get update") {
			issues = append(issues, "Use 'apt-get update && apt-get install' in single RUN to reduce layers")
		}
	}

	// Check for yum cleanup
	if strings.Contains(strings.ToLower(args), "yum install") {
		if !strings.Contains(args, "yum clean all") {
			issues = append(issues, "Missing 'yum clean all' for CentOS/RHEL images")
		}
	}

	// Check for multiple RUN commands (layer bloat)
	if strings.Count(args, "&&") > 5 {
		issues = append(issues, "Consider splitting very long RUN chains for readability")
	}

	instr.Warnings = issues
	return instr
}

// analyzeCOPY checks COPY/ADD instructions for best practices.
func (a *Analyzer) analyzeCOPY(instr Instruction, cmd string, args string) Instruction {
	issues := make([]string, 0)

	if cmd == "ADD" {
		issues = append(issues, "Prefer COPY over ADD unless you specifically need URL or tar extraction")
	}

	// Warn about copying entire working directory
	if strings.Contains(args, ". .") || strings.Contains(args, ".") {
		issues = append(issues, "Consider using .dockerignore to exclude unnecessary files (node_modules, .git, etc.)")
	}

	instr.Warnings = issues
	return instr
}

// analyzeUSER checks for running as non-root.
func (a *Analyzer) analyzeUSER(instr Instruction, args string) Instruction {
	issues := make([]string, 0)

	if args == "root" || args == "0" {
		issues = append(issues, "Avoid running as root; create a non-privileged user for security")
	}

	instr.Warnings = issues
	return instr
}

// generateRecommendations creates actionable suggestions for improvement.
func (a *Analyzer) generateRecommendations(analysis *DockerfileAnalysis) []string {
	recs := make([]string, 0)

	if !analysis.IsMultiStage {
		recs = append(recs, "Consider using multi-stage builds to reduce final image size")
	}

	// Check for .dockerignore
	if len(analysis.Instructions) > 0 {
		// If we see COPY . . or similar, suggest .dockerignore
		hasCopyAll := false
		for _, instr := range analysis.Instructions {
			if instr.Command == "COPY" && strings.Contains(instr.Args, ".") {
				hasCopyAll = true
				break
			}
		}
		if hasCopyAll {
			recs = append(recs, "Create a .dockerignore file to exclude node_modules, .git, test files, etc.")
		}
	}

	// Check for layer count
	layerCount := 0
	for _, instr := range analysis.Instructions {
		if instr.Command == "RUN" || instr.Command == "COPY" || instr.Command == "ADD" {
			layerCount++
		}
	}
	if layerCount > 20 {
		recs = append(recs, fmt.Sprintf("High layer count (%d): combine RUN commands with && to reduce layers", layerCount))
	}

	// Suggest base image best practices
	if analysis.BaseImage != "" {
		baseUpper := strings.ToUpper(analysis.BaseImage)
		if strings.Contains(baseUpper, "UBUNTU") || strings.Contains(baseUpper, "DEBIAN") {
			recs = append(recs, "Consider lightweight base images (alpine, distroless) to reduce bloat")
		}
		if !strings.Contains(analysis.BaseImage, ":") {
			recs = append(recs, "Specify explicit base image tag (not 'latest') for reproducibility")
		}
	}

	return recs
}

// GenerateOptimizedDockerfile suggests an improved version.
func GenerateOptimizedDockerfile(analysis *DockerfileAnalysis, language string) string {
	builder := strings.Builder{}

	// Suggest multi-stage if not already present
	if !analysis.IsMultiStage && language == "java" {
		builder.WriteString("# Multi-stage build recommended for Java\n")
		builder.WriteString("FROM maven:3.8-openjdk-17 AS build\n")
		builder.WriteString("WORKDIR /app\n")
		builder.WriteString("COPY pom.xml .\n")
		builder.WriteString("RUN mvn dependency:resolve\n")
		builder.WriteString("COPY src ./src\n")
		builder.WriteString("RUN mvn clean package -DskipTests\n\n")
		builder.WriteString("FROM openjdk:17-slim\n")
		builder.WriteString("RUN useradd -m -u 1000 appuser\n")
		builder.WriteString("COPY --from=build /app/target/*.jar app.jar\n")
		builder.WriteString("USER appuser\n")
		builder.WriteString("ENTRYPOINT [\"java\", \"-jar\", \"app.jar\"]\n")
	} else if !analysis.IsMultiStage && language == "nodejs" {
		builder.WriteString("# Multi-stage build for Node.js\n")
		builder.WriteString("FROM node:18-alpine AS build\n")
		builder.WriteString("WORKDIR /app\n")
		builder.WriteString("COPY package*.json .\n")
		builder.WriteString("RUN npm ci --only=production\n\n")
		builder.WriteString("FROM node:18-alpine\n")
		builder.WriteString("RUN addgroup -g 1000 appuser && adduser -u 1000 -G appuser appuser\n")
		builder.WriteString("WORKDIR /app\n")
		builder.WriteString("COPY --from=build /app/node_modules ./node_modules\n")
		builder.WriteString("COPY --chown=appuser:appuser . .\n")
		builder.WriteString("USER appuser\n")
		builder.WriteString("CMD [\"node\", \"index.js\"]\n")
	}

	return builder.String()
}

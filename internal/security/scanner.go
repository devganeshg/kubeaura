// Package security provides container image scanning and CVE analysis.
package security

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CVE represents a known security vulnerability.
type CVE struct {
	ID          string    `json:"id"`       // CVE-2024-1234
	Severity    string    `json:"severity"` // CRITICAL, HIGH, MEDIUM, LOW
	Score       float32   `json:"score"`    // 0.0-10.0 (CVSS)
	Title       string    `json:"title"`
	Description string    `json:"description"`
	PublishedAt time.Time `json:"publishedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	References  []string  `json:"references"` // Links to CVE databases
	Package     string    `json:"package"`    // Affected package name
	PackageVer  string    `json:"packageVer"`
	FixVer      string    `json:"fixVer,omitempty"` // Available fix version (if any)
}

// VulnerableDependency represents a package with known CVEs.
type VulnerableDependency struct {
	Package       string `json:"package"`
	Version       string `json:"version"`
	CVEs          []CVE  `json:"cves"`
	CriticalCount int    `json:"criticalCount"`
	HighCount     int    `json:"highCount"`
	MediumCount   int    `json:"mediumCount"`
}

// ImageScan represents the result of scanning a container image.
type ImageScan struct {
	ImageName       string                 `json:"imageName"` // "k8s/kubeaura:v1.2.3"
	Digest          string                 `json:"digest"`
	ScannedAt       time.Time              `json:"scannedAt"`
	ScannerVersion  string                 `json:"scannerVersion"` // "trivy v0.45.0"
	Status          string                 `json:"status"`         // "completed", "failed"
	Vulnerabilities []VulnerableDependency `json:"vulnerabilities"`
	LayerAnalysis   []LayerVulnerabilities `json:"layerAnalysis,omitempty"`
	Summary         struct {
		TotalCVEs    int `json:"totalCves"`
		CriticalCVEs int `json:"criticalCves"`
		HighCVEs     int `json:"highCves"`
		MediumCVEs   int `json:"mediumCves"`
		LowCVEs      int `json:"lowCves"`
	} `json:"summary"`
	Compliance struct {
		IsCompliant bool     `json:"isCompliant"` // Can deploy to production?
		Violations  []string `json:"violations"`  // Policy violations (if any)
	} `json:"compliance"`
}

// LayerVulnerabilities represents vulnerabilities in a specific image layer.
type LayerVulnerabilities struct {
	LayerDigest string   `json:"layerDigest"`
	LayerSize   int64    `json:"layerSize"`
	Packages    []string `json:"packages"` // Packages installed in this layer
	CVECount    int      `json:"cveCount"`
}

// Policy defines compliance rules for image scanning.
type Policy struct {
	Name              string    `json:"name"`              // "strict-production"
	MaxCriticalCVEs   int       `json:"maxCriticalCves"`   // 0 for prod
	MaxHighCVEs       int       `json:"maxHighCves"`       // 0 for prod
	MaxMediumCVEs     int       `json:"maxMediumCves"`     // 10 for dev
	AllowedRegistries []string  `json:"allowedRegistries"` // e.g., ["artifactory.example.com"]
	AllowedBaseImages []string  `json:"allowedBaseImages"` // Approved base images
	RejectOS          []string  `json:"rejectOs"`          // e.g., ["windows/nanoserver"] (if unsupported)
	RequireSignatures bool      `json:"requireSignatures"` // Image must be signed
	CreatedAt         time.Time `json:"createdAt"`
}

// ScanClient scans container images for vulnerabilities.
type ScanClient interface {
	// ScanImage triggers a scan of a container image and returns results.
	ScanImage(ctx context.Context, imageRef string) (*ImageScan, error)

	// GetScanResult retrieves a previously completed scan.
	GetScanResult(ctx context.Context, imageRef string) (*ImageScan, error)

	// ListScans returns recent scans (paginated).
	ListScans(ctx context.Context, limit int, offset int) ([]ImageScan, error)
}

// Remediation suggests fixes for a vulnerability.
type Remediation struct {
	CVE            string `json:"cve"`
	Package        string `json:"package"`
	CurrentVer     string `json:"currentVer"`
	RecommendedVer string `json:"recommendedVer"`
	Effort         string `json:"effort"` // "low", "medium", "high"
	Risk           string `json:"risk"`   // "minimal", "moderate", "high"
	TestRequired   bool   `json:"testRequired"`
}

// GetRemediationPlan returns suggested fixes for vulnerable packages in an image.
func GetRemediationPlan(scan *ImageScan) []Remediation {
	plan := make([]Remediation, 0)
	for _, dep := range scan.Vulnerabilities {
		for _, cve := range dep.CVEs {
			if cve.FixVer != "" && cve.FixVer != dep.Version {
				plan = append(plan, Remediation{
					CVE:            cve.ID,
					Package:        dep.Package,
					CurrentVer:     dep.Version,
					RecommendedVer: cve.FixVer,
					Effort:         estimateEffort(cve.Severity),
					Risk:           estimateRisk(cve.Severity),
					TestRequired:   cve.Severity == "CRITICAL",
				})
			}
		}
	}
	return plan
}

// CheckCompliance verifies an image scan against a policy.
func CheckCompliance(scan *ImageScan, policy Policy) (bool, []string) {
	violations := make([]string, 0)

	if scan.Summary.CriticalCVEs > policy.MaxCriticalCVEs {
		violations = append(violations,
			fmt.Sprintf("Critical CVEs: %d > allowed %d", scan.Summary.CriticalCVEs, policy.MaxCriticalCVEs))
	}
	if scan.Summary.HighCVEs > policy.MaxHighCVEs {
		violations = append(violations,
			fmt.Sprintf("High CVEs: %d > allowed %d", scan.Summary.HighCVEs, policy.MaxHighCVEs))
	}
	if scan.Summary.MediumCVEs > policy.MaxMediumCVEs {
		violations = append(violations,
			fmt.Sprintf("Medium CVEs: %d > allowed %d", scan.Summary.MediumCVEs, policy.MaxMediumCVEs))
	}

	return len(violations) == 0, violations
}

// BuiltinPolicies are the named bars an image can be checked against. They are
// deliberately few and self-explanatory: a policy an operator has to look up is
// a policy they will not trust the verdict of.
var BuiltinPolicies = map[string]Policy{
	"strict-production": {
		Name:            "strict-production",
		MaxCriticalCVEs: 0,
		MaxHighCVEs:     0,
		MaxMediumCVEs:   10,
	},
	"balanced-staging": {
		Name:            "balanced-staging",
		MaxCriticalCVEs: 0,
		MaxHighCVEs:     5,
		MaxMediumCVEs:   50,
	},
	"permissive-dev": {
		Name:            "permissive-dev",
		MaxCriticalCVEs: 5,
		MaxHighCVEs:     25,
		MaxMediumCVEs:   200,
	},
}

// DefaultPolicyName is applied when a compliance check names no policy.
const DefaultPolicyName = "strict-production"

// PolicyByName returns a built-in policy. An unknown name is an error rather
// than a silent fall-through to a permissive default.
func PolicyByName(name string) (Policy, error) {
	if name == "" {
		name = DefaultPolicyName
	}
	p, ok := BuiltinPolicies[name]
	if !ok {
		names := make([]string, 0, len(BuiltinPolicies))
		for n := range BuiltinPolicies {
			names = append(names, n)
		}
		sort.Strings(names)
		return Policy{}, fmt.Errorf("unknown policy %q: choose one of %s", name, strings.Join(names, ", "))
	}
	return p, nil
}

// helper: estimate remediation effort
func estimateEffort(severity string) string {
	switch severity {
	case "CRITICAL":
		return "high"
	case "HIGH":
		return "medium"
	default:
		return "low"
	}
}

// helper: estimate remediation risk
func estimateRisk(severity string) string {
	switch severity {
	case "CRITICAL":
		return "high"
	case "HIGH":
		return "moderate"
	default:
		return "minimal"
	}
}

package security

import (
	"testing"
)

func TestCVE_Severity(t *testing.T) {
	tests := []struct {
		name     string
		severity string
		expected string
	}{
		{"critical", "CRITICAL", "CRITICAL"},
		{"high", "HIGH", "HIGH"},
		{"medium", "MEDIUM", "MEDIUM"},
		{"low", "LOW", "LOW"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cve := &CVE{
				ID:       "CVE-2024-1234",
				Severity: tt.severity,
			}

			if cve.Severity != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, cve.Severity)
			}
		})
	}
}

func TestImageScan_Summary(t *testing.T) {
	scan := &ImageScan{
		ImageName: "test:v1",
		Status:    "completed",
	}
	scan.Summary.CriticalCVEs = 2
	scan.Summary.HighCVEs = 5
	scan.Summary.MediumCVEs = 10

	if scan.Summary.TotalCVEs != 0 { // We didn't set TotalCVEs
		t.Logf("Total CVEs: %d", scan.Summary.TotalCVEs)
	}

	if scan.Summary.CriticalCVEs != 2 {
		t.Error("critical count mismatch")
	}

	if scan.Summary.HighCVEs != 5 {
		t.Error("high count mismatch")
	}
}

func TestCheckCompliance(t *testing.T) {
	scan := &ImageScan{
		ImageName: "app:v1",
		Status:    "completed",
	}
	scan.Summary.CriticalCVEs = 1
	scan.Summary.HighCVEs = 3
	scan.Summary.MediumCVEs = 5

	policy := Policy{
		Name:            "production",
		MaxCriticalCVEs: 0,
		MaxHighCVEs:     0,
		MaxMediumCVEs:   0,
	}

	compliant, violations := CheckCompliance(scan, policy)

	if compliant {
		t.Error("expected compliance check to fail")
	}

	if len(violations) == 0 {
		t.Error("expected violations")
	}

	t.Logf("Violations: %v", violations)
}

func TestGetRemediationPlan(t *testing.T) {
	scan := &ImageScan{
		ImageName: "app:v1",
		Vulnerabilities: []VulnerableDependency{
			{
				Package: "openssl",
				Version: "1.1.1",
				CVEs: []CVE{
					{
						ID:       "CVE-2024-0001",
						Severity: "CRITICAL",
						FixVer:   "1.1.2",
					},
				},
			},
		},
	}

	plan := GetRemediationPlan(scan)

	if len(plan) != 1 {
		t.Errorf("expected 1 remediation, got %d", len(plan))
	}

	if plan[0].Package != "openssl" {
		t.Error("package mismatch")
	}

	if plan[0].RecommendedVer != "1.1.2" {
		t.Error("recommended version mismatch")
	}

	if !plan[0].TestRequired {
		t.Error("test should be required for CRITICAL CVE")
	}
}

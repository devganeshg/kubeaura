package artifactory

import (
	"testing"
)

func TestImageManifest(t *testing.T) {
	manifest := &ImageManifest{
		Name:   "k8s/kubemind",
		Tag:    "v1.2.3",
		Digest: "sha256:abcd1234",
		Size:   1024 * 1024, // 1MB
	}

	if manifest.Name != "k8s/kubemind" {
		t.Error("name mismatch")
	}

	if manifest.Tag != "v1.2.3" {
		t.Error("tag mismatch")
	}

	if manifest.Size != 1024*1024 {
		t.Error("size mismatch")
	}
}

func TestRepository(t *testing.T) {
	repo := &Repository{
		Name:        "k8s/backend",
		Type:        "LOCAL",
		PackageType: "Docker",
		Images: []ImageManifest{
			{Name: "k8s/backend", Tag: "latest"},
			{Name: "k8s/backend", Tag: "develop"},
		},
	}

	if len(repo.Images) != 2 {
		t.Error("expected 2 images")
	}

	if repo.Type != "LOCAL" {
		t.Error("type should be LOCAL")
	}

	if repo.PackageType != "Docker" {
		t.Error("package type should be Docker")
	}
}

func TestConfig(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		valid bool
	}{
		{
			name:  "valid config",
			cfg:   Config{BaseURL: "https://artifactory.example.com", User: "user", APIKey: "key"},
			valid: true,
		},
		{
			name:  "missing base URL",
			cfg:   Config{User: "user", APIKey: "key"},
			valid: false,
		},
		{
			name:  "missing credentials",
			cfg:   Config{BaseURL: "https://artifactory.example.com"},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewClient(tt.cfg)
			if tt.valid && err != nil {
				t.Errorf("expected no error for valid config, got: %v", err)
			}
			if !tt.valid && err == nil {
				t.Error("expected error for invalid config")
			}
		})
	}
}

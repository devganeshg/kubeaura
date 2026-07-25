// Package artifactory provides integration with Artifactory container registries.
package artifactory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client wraps Artifactory API for registry operations.
type Client struct {
	baseURL string
	user    string
	apiKey  string
	httpCli *http.Client
}

// ImageManifest represents container image metadata in registry.
type ImageManifest struct {
	Name         string    `json:"name"`   // e.g., "k8s/kubemind"
	Tag          string    `json:"tag"`    // e.g., "v1.2.3" or "develop"
	Digest       string    `json:"digest"` // SHA256
	Size         int64     `json:"size"`
	PushedAt     time.Time `json:"pushedAt"`
	PulledAt     time.Time `json:"pulledAt,omitempty"`
	Created      time.Time `json:"created"`
	DockerType   string    `json:"dockerType"`   // "application/vnd.docker.distribution.manifest.v2+json"
	Architecture string    `json:"architecture"` // "amd64", "arm64", etc.
	OS           string    `json:"os"`
}

// Repository represents an image repository in registry.
type Repository struct {
	Name        string          `json:"name"`        // "k8s/code-samples/backend-singleapp"
	Type        string          `json:"type"`        // "LOCAL", "REMOTE", "VIRTUAL"
	PackageType string          `json:"packageType"` // "Docker"
	Path        string          `json:"path"`
	Images      []ImageManifest `json:"images"`
	Stats       struct {
		ImageCount   int       `json:"imageCount"`
		LastModified time.Time `json:"lastModified"`
		TotalSize    int64     `json:"totalSize"`
		PullCount    int       `json:"pullCount"`
	} `json:"stats"`
}

// LayerInfo represents a Docker layer within an image.
type LayerInfo struct {
	Digest    string   `json:"digest"`
	Size      int64    `json:"size"`
	MediaType string   `json:"mediaType"`
	URLs      []string `json:"urls"`
}

// Config holds Artifactory connection settings.
type Config struct {
	BaseURL string // e.g., "https://docker.artifactory.example.com"
	User    string // LDAP ID
	APIKey  string // Artifactory API token
}

// NewClient creates an Artifactory registry client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("artifactory: BaseURL required")
	}
	if cfg.User == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("artifactory: User and APIKey required")
	}

	return &Client{
		baseURL: cfg.BaseURL,
		user:    cfg.User,
		apiKey:  cfg.APIKey,
		httpCli: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// ListRepositories returns all available Docker repositories.
func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	// GET /api/repositories (lists all repos across Artifactory)
	// This is a simplified endpoint; real Artifactory may require different API
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/api/repositories", c.baseURL), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifactory: list repos: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("artifactory: %d %s", resp.StatusCode, string(body))
	}

	var repos []Repository
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		return nil, fmt.Errorf("artifactory: parse repos: %w", err)
	}
	return repos, nil
}

// ListImages lists all image tags in a repository.
func (c *Client) ListImages(ctx context.Context, repoPath string) ([]ImageManifest, error) {
	// GET /v2/{repoPath}/tags/list (Docker Registry V2 API)
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/v2/%s/tags/list", c.baseURL, repoPath), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifactory: list images: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("artifactory: %d %s", resp.StatusCode, string(body))
	}

	var result struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("artifactory: parse images: %w", err)
	}

	images := make([]ImageManifest, 0, len(result.Tags))
	for _, tag := range result.Tags {
		// Fetch manifest metadata for each tag
		if img, err := c.GetImageManifest(ctx, repoPath, tag); err == nil {
			images = append(images, *img)
		}
	}
	return images, nil
}

// GetImageManifest returns detailed metadata for a specific image tag.
func (c *Client) GetImageManifest(ctx context.Context, repoPath, tag string) (*ImageManifest, error) {
	// HEAD /v2/{repoPath}/manifests/{tag} to get digest
	req, err := http.NewRequestWithContext(ctx, "HEAD",
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, repoPath, tag), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artifactory: get manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifactory: manifest %d", resp.StatusCode)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	return &ImageManifest{
		Name:     repoPath,
		Tag:      tag,
		Digest:   digest,
		Created:  time.Now(), // TODO: parse from response headers
		PushedAt: time.Now(),
	}, nil
}

// GetImageConfig returns the image config blob (environment, entrypoint, etc.).
func (c *Client) GetImageConfig(ctx context.Context, repoPath, digest string) (map[string]interface{}, error) {
	// GET /v2/{repoPath}/blobs/{digest}
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/v2/%s/blobs/%s", c.baseURL, repoPath, digest), nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifactory: get config %d", resp.StatusCode)
	}

	var config map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, fmt.Errorf("artifactory: parse config: %w", err)
	}
	return config, nil
}

// DeleteImage removes a specific image tag from the registry.
func (c *Client) DeleteImage(ctx context.Context, repoPath, tag string) error {
	// DELETE /v2/{repoPath}/manifests/{tag}
	req, err := http.NewRequestWithContext(ctx, "DELETE",
		fmt.Sprintf("%s/v2/%s/manifests/%s", c.baseURL, repoPath, tag), nil)
	if err != nil {
		return err
	}
	c.setAuth(req)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("artifactory: delete image %d", resp.StatusCode)
	}
	return nil
}

// helper: set Basic Auth header
func (c *Client) setAuth(req *http.Request) {
	auth := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.apiKey))
	req.Header.Set("Authorization", "Basic "+auth)
}

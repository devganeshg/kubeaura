package k8s

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The write half of Helm support. Installing and upgrading needs chart
// fetching, dependency resolution and templating; rolling back and uninstalling
// need Helm's release bookkeeping to stay consistent. Re-implementing any of
// that against the storage secrets would risk corrupting release state, so
// KubeAura shells out to the helm binary when the operator has one — the same
// detect-don't-install stance it takes with Trivy, Kyverno and Argo CD. When
// helm is absent the read-only views in helm.go still work and the UI simply
// does not offer the write actions.

const helmExecTimeout = 10 * time.Minute

// helmName matches a Kubernetes-style name. Every user-supplied value that
// becomes a helm argument is checked against this (or an equivalent) pattern,
// which is what stops a value like "--kubeconfig=/etc/evil" from being parsed
// as a flag rather than a name.
var helmName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,251}[a-z0-9]$|^[a-z0-9]$`)

// chartRef allows what helm accepts as a chart: repo/chart, an oci:// URL, an
// https:// URL, or a local path. Leading dashes are rejected so a chart
// reference can never be read as a flag.
var chartRef = regexp.MustCompile(`^[A-Za-z0-9._/:@+-]+$`)

type helmCLI struct {
	path    string
	version string
	found   bool
}

var (
	helmOnce sync.Once
	helmInfo helmCLI
)

// helmCLIVersion locates the helm binary once per process and reports its
// version. The lookup is cached because it runs on every release listing.
func helmCLIVersion() (string, bool) {
	helmOnce.Do(func() {
		path, err := exec.LookPath("helm")
		if err != nil {
			return
		}
		helmInfo.path, helmInfo.found = path, true
		cx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cx, path, "version", "--short").Output()
		if err == nil {
			helmInfo.version = strings.TrimSpace(string(out))
		}
	})
	return helmInfo.version, helmInfo.found
}

// HelmOp describes one lifecycle request from the UI.
type HelmOp struct {
	Action    string // install | upgrade | rollback | uninstall
	Release   string
	Namespace string
	Chart     string // install/upgrade only
	Version   string // chart version, install/upgrade only
	Values    string // YAML, install/upgrade only
	Revision  int    // rollback only; 0 means the previous revision
	Wait      bool
	DryRun    bool
	Atomic    bool
	CreateNS  bool
}

// HelmOpResult is what the UI shows after a lifecycle action.
type HelmOpResult struct {
	Action  string `json:"action"`
	Release string `json:"release"`
	Command string `json:"command"` // the helm invocation, for the audit trail
	Output  string `json:"output"`
	DryRun  bool   `json:"dryRun"`
}

// RunHelm validates and executes one lifecycle action against this client's
// context. It returns an error when helm is not installed so the caller can
// surface that as a precondition rather than a failure.
func (c *Client) RunHelm(cx context.Context, op HelmOp) (*HelmOpResult, error) {
	if _, ok := helmCLIVersion(); !ok {
		return nil, fmt.Errorf("the helm binary was not found on PATH; install Helm to enable install, upgrade, rollback and uninstall")
	}
	args, cleanup, err := helmArgs(c.Context, op)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return nil, err
	}

	runCx, cancel := context.WithTimeout(cx, helmExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCx, helmInfo.path, args...)
	// helm reads HELM_* and KUBECONFIG from the environment; inheriting the
	// operator's environment is what makes it behave like their own shell.
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()

	res := &HelmOpResult{
		Action:  op.Action,
		Release: op.Release,
		Command: "helm " + strings.Join(redactHelmArgs(args), " "),
		Output:  strings.TrimSpace(string(out)),
		DryRun:  op.DryRun,
	}
	if runErr != nil {
		if runCx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("helm %s timed out after %s", op.Action, helmExecTimeout)
		}
		// helm's own stderr is far more useful than the exit status.
		msg := res.Output
		if msg == "" {
			msg = runErr.Error()
		}
		return res, fmt.Errorf("helm %s failed: %s", op.Action, msg)
	}
	return res, nil
}

// helmArgs builds the argument list for one operation. When the operation
// carries values YAML it also writes a temp file and returns its cleanup.
func helmArgs(kubeContext string, op HelmOp) ([]string, func(), error) {
	if op.Namespace != "" && !helmName.MatchString(op.Namespace) {
		return nil, nil, fmt.Errorf("invalid namespace %q", op.Namespace)
	}
	if op.Release != "" && !helmName.MatchString(op.Release) {
		return nil, nil, fmt.Errorf("invalid release name %q", op.Release)
	}

	var args []string
	switch op.Action {
	case "install", "upgrade":
		if op.Release == "" {
			return nil, nil, fmt.Errorf("release name is required")
		}
		if op.Chart == "" {
			return nil, nil, fmt.Errorf("chart is required")
		}
		if !chartRef.MatchString(op.Chart) || strings.HasPrefix(op.Chart, "-") {
			return nil, nil, fmt.Errorf("invalid chart reference %q", op.Chart)
		}
		if op.Action == "install" {
			args = []string{"install", op.Release, op.Chart}
		} else {
			// --install makes upgrade idempotent for a release that was
			// removed out from under the UI between listing and acting.
			args = []string{"upgrade", "--install", op.Release, op.Chart}
		}
		if op.Version != "" {
			if !chartRef.MatchString(op.Version) || strings.HasPrefix(op.Version, "-") {
				return nil, nil, fmt.Errorf("invalid chart version %q", op.Version)
			}
			args = append(args, "--version", op.Version)
		}
		if op.CreateNS {
			args = append(args, "--create-namespace")
		}
		if op.Atomic {
			args = append(args, "--atomic")
		}
	case "rollback":
		if op.Release == "" {
			return nil, nil, fmt.Errorf("release name is required")
		}
		args = []string{"rollback", op.Release}
		if op.Revision > 0 {
			args = append(args, fmt.Sprint(op.Revision))
		}
	case "uninstall":
		if op.Release == "" {
			return nil, nil, fmt.Errorf("release name is required")
		}
		args = []string{"uninstall", op.Release}
	default:
		return nil, nil, fmt.Errorf("unsupported helm action %q", op.Action)
	}

	if op.Namespace != "" {
		args = append(args, "--namespace", op.Namespace)
	}
	// "in-cluster" is not a kubeconfig context; in that mode helm uses the
	// in-cluster service account exactly as this process does.
	if kubeContext != "" && kubeContext != "in-cluster" {
		args = append(args, "--kube-context", kubeContext)
	}
	if op.Wait {
		args = append(args, "--wait")
	}
	if op.DryRun {
		args = append(args, "--dry-run")
	}

	var cleanup func()
	if strings.TrimSpace(op.Values) != "" && (op.Action == "install" || op.Action == "upgrade") {
		f, err := os.CreateTemp("", "kubeaura-helm-values-*.yaml")
		if err != nil {
			return nil, nil, fmt.Errorf("write values file: %w", err)
		}
		name := f.Name()
		cleanup = func() { _ = os.Remove(name) }
		if _, err := f.WriteString(op.Values); err != nil {
			f.Close()
			return nil, cleanup, fmt.Errorf("write values file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, cleanup, fmt.Errorf("write values file: %w", err)
		}
		args = append(args, "--values", name)
	}
	return args, cleanup, nil
}

// redactHelmArgs replaces the temp values path in the echoed command, because
// the audit trail should record that values were supplied without pointing at
// a path that no longer exists.
func redactHelmArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "--values" && i+1 < len(out) {
			out[i+1] = "<values.yaml>"
		} else if strings.HasPrefix(a, filepath.Join(os.TempDir(), "kubeaura-helm-values-")) {
			out[i] = "<values.yaml>"
		}
	}
	return out
}

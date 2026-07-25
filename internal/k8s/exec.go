package k8s

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecCommand runs a one-shot command inside a pod container and returns its
// combined stdout+stderr. This is a non-interactive command runner (no TTY) —
// enough to inspect a container (ls, cat, env, ps, curl) from the UI over plain
// HTTP without a websocket/PTY bridge.
func (c *Client) ExecCommand(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("command is required")
	}
	if container == "" {
		if names, err := c.PodContainers(namespace, pod); err == nil && len(names) > 0 {
			container = names[len(names)-1] // default to the last (usually main) container
		}
	}
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.cfg, "POST", req.URL())
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &out, Stderr: &out})
	text := out.String()
	if err != nil {
		// Surface command output alongside the error (e.g. non-zero exit code).
		if text != "" {
			return text, fmt.Errorf("%w", err)
		}
		return "", err
	}
	return text, nil
}

// ParseCommand splits a shell-ish command string into argv. It supports simple
// single/double quoting; it does NOT interpret pipes or globs (those must be run
// via `sh -c`).
func ParseCommand(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var args []string
	var cur strings.Builder
	quote := rune(0)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// newShortID returns a short random hex id used for port-forwards and similar.
func newShortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

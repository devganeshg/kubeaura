package k8s

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForward is one active port-forward the tool is managing. KubeAura holds
// the forward open in-process (a goroutine per forward) and tracks them all so
// the operator has a single dashboard to create and tear them down — matching
// the "Live Port-Forward Tracker" requirement.
type PortForward struct {
	ID         string    `json:"id"`
	Namespace  string    `json:"namespace"`
	Target     string    `json:"target"` // "pod/name" or "service/name"
	Pod        string    `json:"pod"`    // the pod actually forwarded to
	LocalPort  uint16    `json:"localPort"`
	RemotePort uint16    `json:"remotePort"`
	Created    time.Time `json:"created"`
	stop       chan struct{}
}

// StartPortForward opens a forward from localPort (0 = auto-pick) to remotePort
// on a pod. A service target is resolved to one of its ready backing pods.
func (c *Client) StartPortForward(namespace, kind, name string, localPort, remotePort uint16) (*PortForward, error) {
	pod, err := c.resolveForwardPod(namespace, kind, name)
	if err != nil {
		return nil, err
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(c.cfg)
	if err != nil {
		return nil, err
	}
	reqURL := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, reqURL)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}

	fw, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, err
	}
	errCh := make(chan error, 1)
	go func() { errCh <- fw.ForwardPorts() }()

	// Wait for readiness (or an early failure) so we can report the real local port.
	select {
	case <-readyCh:
	case err := <-errCh:
		return nil, fmt.Errorf("port-forward failed: %w", err)
	case <-time.After(10 * time.Second):
		close(stopCh)
		return nil, fmt.Errorf("port-forward to %s/%s timed out", namespace, pod)
	}

	fwPorts, err := fw.GetPorts()
	if err != nil || len(fwPorts) == 0 {
		close(stopCh)
		return nil, fmt.Errorf("could not read forwarded ports: %v", err)
	}

	pf := &PortForward{
		ID: newPFID(), Namespace: namespace, Target: kind + "/" + name, Pod: pod,
		LocalPort: fwPorts[0].Local, RemotePort: fwPorts[0].Remote, Created: time.Now(), stop: stopCh,
	}
	c.pfMu.Lock()
	c.pf[pf.ID] = pf
	c.pfMu.Unlock()

	// Auto-cleanup registry entry if the forward dies on its own.
	go func() { <-errCh; c.pfMu.Lock(); delete(c.pf, pf.ID); c.pfMu.Unlock() }()
	return pf, nil
}

// resolveForwardPod returns a pod name to forward to. For a service it picks the
// first ready pod behind the selector.
func (c *Client) resolveForwardPod(namespace, kind, name string) (string, error) {
	cx, cancel := ctx()
	defer cancel()
	switch kind {
	case "service", "services", "svc":
		svc, err := c.cs.CoreV1().Services(namespace).Get(cx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if len(svc.Spec.Selector) == 0 {
			return "", fmt.Errorf("service %s has no selector to resolve a pod", name)
		}
		sel := labels.SelectorFromSet(svc.Spec.Selector).String()
		pods, err := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return "", err
		}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning {
				return p.Name, nil
			}
		}
		return "", fmt.Errorf("no running pods behind service %s", name)
	default: // pod
		return name, nil
	}
}

// StopPortForward tears down a forward by id.
func (c *Client) StopPortForward(id string) error {
	c.pfMu.Lock()
	defer c.pfMu.Unlock()
	pf, ok := c.pf[id]
	if !ok {
		return fmt.Errorf("no such port-forward %q", id)
	}
	close(pf.stop)
	delete(c.pf, id)
	return nil
}

// PortForwards lists the active forwards, newest first.
func (c *Client) PortForwards() []PortForward {
	c.pfMu.Lock()
	defer c.pfMu.Unlock()
	out := make([]PortForward, 0, len(c.pf))
	for _, pf := range c.pf {
		out = append(out, *pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func newPFID() string { return newShortID() }

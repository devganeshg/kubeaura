package k8s

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// HygieneFinding is a read-only signal that a resource may be unused.
type HygieneFinding struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Reason    string `json:"reason"`
}

// HygieneReport contains candidate unused resources and checks that could not
// run because the active credentials lack access.
type HygieneReport struct {
	Namespace string           `json:"namespace"`
	Findings  []HygieneFinding `json:"findings"`
	Warnings  []string         `json:"warnings"`
}

// Hygiene reports a small set of explainable unused-resource signals. It does
// not mutate or delete cluster resources; findings are candidates for review.
func (c *Client) Hygiene(namespace string) (*HygieneReport, error) {
	cx, cancel := ctx()
	defer cancel()
	report := &HygieneReport{
		Namespace: namespace,
		Findings:  []HygieneFinding{},
		Warnings:  []string{},
	}
	add := func(kind, name, ns, reason string) {
		report.Findings = append(report.Findings, HygieneFinding{
			Kind: kind, Name: name, Namespace: ns, Reason: reason,
		})
	}

	pods, podsErr := c.cs.CoreV1().Pods(namespace).List(cx, metav1.ListOptions{})
	if podsErr != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("Pod-based checks unavailable: %v", podsErr))
	}
	if podsErr == nil {
		mountedClaims := map[string]bool{}
		for _, pod := range pods.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil {
					mountedClaims[pod.Namespace+"/"+volume.PersistentVolumeClaim.ClaimName] = true
				}
			}
		}
		claims, err := c.cs.CoreV1().PersistentVolumeClaims(namespace).List(cx, metav1.ListOptions{})
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Sprintf("PVC check unavailable: %v", err))
		} else {
			for _, claim := range claims.Items {
				if !mountedClaims[claim.Namespace+"/"+claim.Name] {
					add("PVC", claim.Name, claim.Namespace, "Not mounted by any pod")
				}
			}
		}
	}

	services, err := c.cs.CoreV1().Services(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("Service check unavailable: %v", err))
	} else if podsErr == nil {
		for _, service := range services.Items {
			if service.Spec.Type == corev1.ServiceTypeExternalName || len(service.Spec.Selector) == 0 {
				continue
			}
			selector := labels.Set(service.Spec.Selector).AsSelector()
			matched := false
			for _, pod := range pods.Items {
				if pod.Namespace == service.Namespace && selector.Matches(labels.Set(pod.Labels)) {
					matched = true
					break
				}
			}
			if !matched {
				add("Service", service.Name, service.Namespace, "Selector matches no pods")
			}
		}
	}

	deployments, err := c.cs.AppsV1().Deployments(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("Deployment check unavailable: %v", err))
	} else {
		for _, deployment := range deployments.Items {
			if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
				add("Deployment", deployment.Name, deployment.Namespace, "Desired replicas set to zero")
			}
		}
	}

	statefulSets, err := c.cs.AppsV1().StatefulSets(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("StatefulSet check unavailable: %v", err))
	} else {
		for _, statefulSet := range statefulSets.Items {
			if statefulSet.Spec.Replicas != nil && *statefulSet.Spec.Replicas == 0 {
				add("StatefulSet", statefulSet.Name, statefulSet.Namespace, "Desired replicas set to zero")
			}
		}
	}

	sort.Slice(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	return report, nil
}
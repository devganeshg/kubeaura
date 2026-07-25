package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Autoscaling visibility (CNCF roadmap Phase 2, item 5): HPAs come from the
// core autoscaling/v2 API and are always evaluated; KEDA ScaledObjects are
// read only when the CRD exists — detect-don't-install.
var scaledObjectGVRs = []schema.GroupVersionResource{
	{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"},
}

// HPARow is the normalized view of one HorizontalPodAutoscaler.
type HPARow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"` // "Deployment/api"
	Min       int32  `json:"min"`
	Max       int32  `json:"max"`
	Current   int32  `json:"current"`
	Desired   int32  `json:"desired"`
	Metrics   string `json:"metrics"` // e.g. "cpu 63%/80%"
	Status    string `json:"status"`  // AtMax|Scaling|Stable|Inactive
	Message   string `json:"message"` // condition detail for troubleshooting
	Age       string `json:"age"`
}

// ScaledObjectRow is the normalized view of one KEDA ScaledObject.
type ScaledObjectRow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Target    string `json:"target"`
	Triggers  string `json:"triggers"` // comma-joined trigger types
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Status    string `json:"status"` // Ready|NotReady|Unknown
	Age       string `json:"age"`
}

// AutoscalingResult is the /api/autoscaling payload.
type AutoscalingResult struct {
	KedaInstalled bool              `json:"kedaInstalled"`
	HPAs          []HPARow          `json:"hpas"`
	ScaledObjects []ScaledObjectRow `json:"scaledObjects"`
}

// Autoscaling lists HPAs (always) and KEDA ScaledObjects (when present).
func (c *Client) Autoscaling(cx context.Context, namespace string) (*AutoscalingResult, error) {
	res := &AutoscalingResult{HPAs: []HPARow{}, ScaledObjects: []ScaledObjectRow{}}

	list, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, h := range list.Items {
		res.HPAs = append(res.HPAs, hpaRow(h))
	}
	sort.Slice(res.HPAs, func(i, j int) bool {
		a, b := res.HPAs[i], res.HPAs[j]
		rank := map[string]int{"AtMax": 0, "Scaling": 1, "Inactive": 2, "Stable": 3}
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		return a.Namespace+a.Name < b.Namespace+b.Name
	})

	if dyn, err := dynamic.NewForConfig(c.cfg); err == nil {
		if items, ok := listFirstAvailable(cx, dyn, namespace, scaledObjectGVRs); ok {
			res.KedaInstalled = true
			for _, it := range items {
				res.ScaledObjects = append(res.ScaledObjects, scaledObjectRow(it))
			}
		}
	}
	return res, nil
}

func hpaRow(h autoscalingv2.HorizontalPodAutoscaler) HPARow {
	min := int32(1)
	if h.Spec.MinReplicas != nil {
		min = *h.Spec.MinReplicas
	}
	status := "Stable"
	msg := ""
	switch {
	case h.Status.CurrentReplicas >= h.Spec.MaxReplicas:
		status = "AtMax"
	case h.Status.DesiredReplicas != h.Status.CurrentReplicas:
		status = "Scaling"
	}
	for _, cond := range h.Status.Conditions {
		if cond.Type == autoscalingv2.ScalingActive && cond.Status != "True" {
			status = "Inactive"
			msg = cond.Message
		}
		if cond.Type == autoscalingv2.AbleToScale && cond.Status != "True" {
			msg = cond.Message
		}
	}
	return HPARow{
		Namespace: h.Namespace, Name: h.Name,
		Target: fmt.Sprintf("%s/%s", h.Spec.ScaleTargetRef.Kind, h.Spec.ScaleTargetRef.Name),
		Min:    min, Max: h.Spec.MaxReplicas,
		Current: h.Status.CurrentReplicas, Desired: h.Status.DesiredReplicas,
		Metrics: hpaMetrics(h), Status: status, Message: msg,
		Age: age(h.CreationTimestamp),
	}
}

// hpaMetrics renders "cpu 63%/80%"-style summaries for resource metrics and a
// generic count for anything more exotic.
func hpaMetrics(h autoscalingv2.HorizontalPodAutoscaler) string {
	currentByName := map[string]int32{}
	for _, m := range h.Status.CurrentMetrics {
		if m.Resource != nil && m.Resource.Current.AverageUtilization != nil {
			currentByName[string(m.Resource.Name)] = *m.Resource.Current.AverageUtilization
		}
	}
	parts := []string{}
	other := 0
	for _, m := range h.Spec.Metrics {
		if m.Resource != nil && m.Resource.Target.AverageUtilization != nil {
			cur := "?"
			if v, ok := currentByName[string(m.Resource.Name)]; ok {
				cur = fmt.Sprintf("%d", v)
			}
			parts = append(parts, fmt.Sprintf("%s %s%%/%d%%", m.Resource.Name, cur, *m.Resource.Target.AverageUtilization))
			continue
		}
		other++
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("+%d custom", other))
	}
	if len(parts) == 0 {
		return "no metrics"
	}
	return strings.Join(parts, ", ")
}

func scaledObjectRow(it unstructured.Unstructured) ScaledObjectRow {
	target, _, _ := unstructured.NestedString(it.Object, "spec", "scaleTargetRef", "name")
	min, _, _ := unstructured.NestedInt64(it.Object, "spec", "minReplicaCount")
	max, found, _ := unstructured.NestedInt64(it.Object, "spec", "maxReplicaCount")
	if !found {
		max = 100 // KEDA's documented default
	}
	types := []string{}
	if triggers, found, _ := unstructured.NestedSlice(it.Object, "spec", "triggers"); found {
		for _, t := range triggers {
			if m, ok := t.(map[string]interface{}); ok {
				tt, _, _ := unstructured.NestedString(m, "type")
				if tt != "" {
					types = append(types, tt)
				}
			}
		}
	}
	status, _ := fluxReadyCondition(it) // same Ready-condition shape as Flux
	return ScaledObjectRow{
		Namespace: it.GetNamespace(), Name: it.GetName(),
		Target: target, Triggers: strings.Join(types, ", "),
		Min: min, Max: max, Status: status,
		Age: age(it.GetCreationTimestamp()),
	}
}

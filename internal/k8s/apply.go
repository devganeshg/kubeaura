package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	sigyaml "sigs.k8s.io/yaml"
)

// copyLimited copies at most max bytes from src to dst.
func copyLimited(dst io.Writer, src io.Reader, max int64) (int64, error) {
	return io.Copy(dst, io.LimitReader(src, max))
}

// ApplyYAML applies one or more YAML documents (server-side apply) and returns
// a per-document result summary. It builds a discovery-backed REST mapper so any
// installed kind (including CRDs) can be applied.
func (c *Client) ApplyYAML(yamlText string) ([]string, error) {
	cfg := c.cfg
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	cx, cancel := ctx()
	defer cancel()

	var results []string
	for _, doc := range splitYAML(yamlText) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		var m map[string]interface{}
		if err := sigyaml.Unmarshal([]byte(doc), &m); err != nil {
			return results, fmt.Errorf("parse yaml: %w", err)
		}
		obj.Object = m
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return results, fmt.Errorf("no mapping for %s: %w", gvk, err)
		}
		var ri dynamic.ResourceInterface
		if mapping.Scope.Name() == "namespace" {
			ns := obj.GetNamespace()
			if ns == "" {
				ns = "default"
				obj.SetNamespace(ns)
			}
			ri = dyn.Resource(mapping.Resource).Namespace(ns)
		} else {
			ri = dyn.Resource(mapping.Resource)
		}
		results = append(results, applyOne(cx, ri, obj, gvk))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no valid Kubernetes objects found in YAML")
	}
	return results, nil
}

func applyOne(cx context.Context, ri dynamic.ResourceInterface, obj *unstructured.Unstructured, gvk schema.GroupVersionKind) string {
	name := obj.GetName()
	_, err := ri.Create(cx, obj, metav1.CreateOptions{})
	if err == nil {
		return fmt.Sprintf("created %s/%s", gvk.Kind, name)
	}
	if errors.IsAlreadyExists(err) {
		// Fall back to a server-side apply patch for updates.
		data, mErr := obj.MarshalJSON()
		if mErr != nil {
			return fmt.Sprintf("error %s/%s: %v", gvk.Kind, name, mErr)
		}
		_, pErr := ri.Patch(cx, name, types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "kubeaura"})
		if pErr != nil {
			return fmt.Sprintf("error updating %s/%s: %v", gvk.Kind, name, pErr)
		}
		return fmt.Sprintf("configured %s/%s", gvk.Kind, name)
	}
	return fmt.Sprintf("error %s/%s: %v", gvk.Kind, name, err)
}

// DiffDoc is the before/after for one object in a proposed apply.
type DiffDoc struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Action    string `json:"action"` // create | update
	Live      string `json:"live"`   // current object YAML ("" for create)
	Proposed  string `json:"proposed"`
}

// DiffYAML computes what a server-side apply of yamlText would change, via a
// dry-run — the same idea as `kubectl diff`. For existing objects it returns the
// live YAML and the dry-run result so the UI can render a line diff; for new
// objects it returns the proposed YAML with an empty "live".
func (c *Client) DiffYAML(yamlText string) ([]DiffDoc, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	cx, cancel := ctx()
	defer cancel()

	var out []DiffDoc
	for _, doc := range splitYAML(yamlText) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]interface{}
		if err := sigyaml.Unmarshal([]byte(doc), &m); err != nil {
			return out, fmt.Errorf("parse yaml: %w", err)
		}
		obj := &unstructured.Unstructured{Object: m}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" {
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return out, fmt.Errorf("no mapping for %s: %w", gvk, err)
		}
		ns := obj.GetNamespace()
		var ri dynamic.ResourceInterface
		if mapping.Scope.Name() == "namespace" {
			if ns == "" {
				ns = "default"
				obj.SetNamespace(ns)
			}
			ri = dyn.Resource(mapping.Resource).Namespace(ns)
		} else {
			ri = dyn.Resource(mapping.Resource)
		}
		d := DiffDoc{Kind: gvk.Kind, Name: obj.GetName(), Namespace: ns}
		live, getErr := ri.Get(cx, obj.GetName(), metav1.GetOptions{})
		data, _ := obj.MarshalJSON()
		force := true
		dryApplied, applyErr := ri.Patch(cx, obj.GetName(), types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: "kubeaura", DryRun: []string{metav1.DryRunAll}, Force: &force})
		if getErr != nil {
			d.Action = "create"
			if applyErr == nil {
				d.Proposed = cleanYAML(dryApplied)
			} else {
				d.Proposed = strings.TrimSpace(doc)
			}
		} else {
			d.Action = "update"
			d.Live = cleanYAML(live)
			if applyErr == nil {
				d.Proposed = cleanYAML(dryApplied)
			} else {
				d.Proposed = d.Live // apply failed; show no change rather than error out
			}
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid Kubernetes objects found in YAML")
	}
	return out, nil
}

// cleanYAML strips server-managed noise so diffs show only meaningful fields.
func cleanYAML(obj *unstructured.Unstructured) string {
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(obj.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(obj.Object, "metadata", "uid")
	unstructured.RemoveNestedField(obj.Object, "metadata", "generation")
	unstructured.RemoveNestedField(obj.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(obj.Object, "status")
	b, _ := sigyaml.Marshal(obj.Object)
	return string(b)
}

// GetYAML fetches a single object and returns it as pretty YAML for the editor.
func (c *Client) GetYAML(kind, namespace, name string) (string, error) {
	cfg := c.cfg
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return "", err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	gk := kindToGroupKind(kind)
	mapping, err := mapper.RESTMapping(gk)
	if err != nil {
		return "", fmt.Errorf("no mapping for %s: %w", kind, err)
	}
	cx, cancel := ctx()
	defer cancel()
	var ri dynamic.ResourceInterface
	if mapping.Scope.Name() == "namespace" {
		ri = dyn.Resource(mapping.Resource).Namespace(namespace)
	} else {
		ri = dyn.Resource(mapping.Resource)
	}
	obj, err := ri.Get(cx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	// Strip noisy server-managed fields for a clean editor experience.
	unstructured.RemoveNestedField(obj.Object, "metadata", "managedFields")
	unstructured.RemoveNestedField(obj.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(obj.Object, "status")
	out, err := sigyaml.Marshal(obj.Object)
	return string(out), err
}

func kindToGroupKind(kind string) schema.GroupKind {
	switch strings.ToLower(kind) {
	case "pod", "pods":
		return schema.GroupKind{Kind: "Pod"}
	case "deployment", "deployments":
		return schema.GroupKind{Group: "apps", Kind: "Deployment"}
	case "statefulset", "statefulsets":
		return schema.GroupKind{Group: "apps", Kind: "StatefulSet"}
	case "daemonset", "daemonsets":
		return schema.GroupKind{Group: "apps", Kind: "DaemonSet"}
	case "service", "services":
		return schema.GroupKind{Kind: "Service"}
	case "ingress", "ingresses":
		return schema.GroupKind{Group: "networking.k8s.io", Kind: "Ingress"}
	case "configmap", "configmaps":
		return schema.GroupKind{Kind: "ConfigMap"}
	case "secret", "secrets":
		return schema.GroupKind{Kind: "Secret"}
	case "job", "jobs":
		return schema.GroupKind{Group: "batch", Kind: "Job"}
	case "cronjob", "cronjobs":
		return schema.GroupKind{Group: "batch", Kind: "CronJob"}
	case "namespace", "namespaces":
		return schema.GroupKind{Kind: "Namespace"}
	case "node", "nodes":
		return schema.GroupKind{Kind: "Node"}
	case "certificate", "certificates":
		return schema.GroupKind{Group: "cert-manager.io", Kind: "Certificate"}
	case "issuer", "issuers":
		return schema.GroupKind{Group: "cert-manager.io", Kind: "Issuer"}
	case "clusterissuer", "clusterissuers":
		return schema.GroupKind{Group: "cert-manager.io", Kind: "ClusterIssuer"}
	default:
		return schema.GroupKind{Kind: kind}
	}
}

// splitYAML splits a multi-document YAML string on "---" separators.
func splitYAML(s string) []string {
	// Normalize and split on lines that are exactly a document separator.
	lines := strings.Split(s, "\n")
	var docs []string
	var cur strings.Builder
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "---" {
			docs = append(docs, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteString(ln)
		cur.WriteString("\n")
	}
	docs = append(docs, cur.String())
	return docs
}

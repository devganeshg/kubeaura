package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	certificatesGVR   = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	issuersGVR        = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "issuers"}
	clusterIssuersGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
)

func (c *Client) listCertificates(cx context.Context, namespace string, opts metav1.ListOptions) (ListResult, error) {
	rows, cont, err := c.listCertManagerResource(cx, namespace, certificatesGVR, "Certificate", opts)
	if err != nil {
		return ListResult{}, err
	}
	if cont != "" {
		return ListResult{Items: rows, Continue: cont}, nil
	}
	return ListResult{Items: rows}, nil
}

func (c *Client) listIssuers(cx context.Context, namespace string, opts metav1.ListOptions) (ListResult, error) {
	rows, cont, err := c.listCertManagerResource(cx, namespace, issuersGVR, "Issuer", opts)
	if err != nil {
		return ListResult{}, err
	}
	if cont != "" {
		return ListResult{Items: rows, Continue: cont}, nil
	}
	return ListResult{Items: rows}, nil
}

func (c *Client) listClusterIssuers(cx context.Context, opts metav1.ListOptions) (ListResult, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return ListResult{}, err
	}
	list, err := dyn.Resource(clusterIssuersGVR).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	rows := make([]Resource, 0, len(list.Items))
	for _, it := range list.Items {
		status := certManagerReadyStatus(it)
		rows = append(rows, Resource{
			Kind:   "ClusterIssuer",
			Name:   it.GetName(),
			Status: status,
			Info:   certManagerInfo("ClusterIssuer", it),
			Age:    age(it.GetCreationTimestamp()),
			Labels: it.GetLabels(),
		})
	}
	return ListResult{Items: rows, Continue: list.GetContinue()}, nil
}

func (c *Client) listCertManagerResource(cx context.Context, namespace string, gvr schema.GroupVersionResource, kind string, opts metav1.ListOptions) ([]Resource, string, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, "", err
	}
	list, err := dyn.Resource(gvr).Namespace(namespace).List(cx, opts)
	if err != nil {
		return nil, "", err
	}
	rows := make([]Resource, 0, len(list.Items))
	for _, it := range list.Items {
		status := certManagerReadyStatus(it)
		rows = append(rows, Resource{
			Kind:      kind,
			Name:      it.GetName(),
			Namespace: it.GetNamespace(),
			Status:    status,
			Info:      certManagerInfo(kind, it),
			Age:       age(it.GetCreationTimestamp()),
			Labels:    it.GetLabels(),
		})
	}
	return rows, list.GetContinue(), nil
}

func certManagerReadyStatus(it unstructured.Unstructured) string {
	conds, found, _ := unstructured.NestedSlice(it.Object, "status", "conditions")
	if !found {
		return "Unknown"
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		if strings.EqualFold(t, "Ready") {
			s, _, _ := unstructured.NestedString(m, "status")
			if s == "True" {
				return "Ready"
			}
			if s == "False" {
				return "NotReady"
			}
		}
	}
	return "Unknown"
}

func certManagerInfo(kind string, it unstructured.Unstructured) string {
	switch kind {
	case "Certificate":
		secret, _, _ := unstructured.NestedString(it.Object, "spec", "secretName")
		dns, _, _ := unstructured.NestedStringSlice(it.Object, "spec", "dnsNames")
		if secret == "" {
			secret = "-"
		}
		if len(dns) == 0 {
			return fmt.Sprintf("secret=%s", secret)
		}
		return fmt.Sprintf("secret=%s, dns=%d", secret, len(dns))
	default:
		refName, _, _ := unstructured.NestedString(it.Object, "spec", "acme", "privateKeySecretRef", "name")
		if refName != "" {
			return "acme key=" + refName
		}
		return "cert-manager issuer"
	}
}

func (c *Client) listHelmReleases(cx context.Context, namespace string, opts metav1.ListOptions) (ListResult, error) {
	list, err := c.cs.CoreV1().Secrets(namespace).List(cx, opts)
	if err != nil {
		return ListResult{}, err
	}
	rows := make([]Resource, 0, len(list.Items))
	for _, s := range list.Items {
		if s.Labels["owner"] != "helm" && !strings.HasPrefix(string(s.Type), "helm.sh/release.v1") {
			continue
		}
		releaseName := s.Labels["name"]
		if releaseName == "" {
			releaseName = helmNameFromSecret(s.Name)
		}
		rev := s.Labels["version"]
		status := strings.Title(strings.ToLower(s.Labels["status"]))
		if status == "" {
			status = "Unknown"
		}
		info := "revision=" + rev
		if rev == "" {
			info = "helm release metadata"
		}
		rows = append(rows, Resource{
			Kind:      "HelmRelease",
			Name:      releaseName,
			Namespace: s.Namespace,
			Status:    status,
			Info:      info,
			Age:       age(s.CreationTimestamp),
			Labels:    s.Labels,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace == rows[j].Namespace {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].Namespace < rows[j].Namespace
	})
	return ListResult{Items: rows, Continue: list.Continue}, nil
}

func helmNameFromSecret(secretName string) string {
	// Helm v3 secret names usually look like sh.helm.release.v1.<name>.v<revision>
	parts := strings.Split(secretName, ".")
	if len(parts) >= 6 && parts[0] == "sh" && parts[1] == "helm" && parts[2] == "release" && parts[3] == "v1" {
		return strings.Join(parts[4:len(parts)-1], ".")
	}
	return secretName
}

func (c *Client) certManagerResourceDetail(cx context.Context, kind, namespace, name string) (*DetailView, error) {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return nil, err
	}
	var gvr schema.GroupVersionResource
	switch strings.ToLower(kind) {
	case "certificate", "certificates":
		gvr = certificatesGVR
	case "issuer", "issuers":
		gvr = issuersGVR
	case "clusterissuer", "clusterissuers":
		gvr = clusterIssuersGVR
	default:
		return nil, fmt.Errorf("unsupported cert-manager kind %q", kind)
	}
	var ri dynamic.ResourceInterface
	if gvr == clusterIssuersGVR {
		ri = dyn.Resource(gvr)
	} else {
		ri = dyn.Resource(gvr).Namespace(namespace)
	}
	obj, err := ri.Get(cx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	k := strings.Title(strings.TrimSuffix(strings.ToLower(kind), "s"))
	meta := metav1.ObjectMeta{
		Name:              obj.GetName(),
		Namespace:         obj.GetNamespace(),
		CreationTimestamp: obj.GetCreationTimestamp(),
		Labels:            obj.GetLabels(),
		Annotations:       obj.GetAnnotations(),
	}
	v := c.base(k, meta, certManagerReadyStatus(*obj))
	fields := []Field{{Key: "API Version", Value: obj.GetAPIVersion()}}
	if k == "Certificate" {
		secret, _, _ := unstructured.NestedString(obj.Object, "spec", "secretName")
		if secret != "" {
			fields = append(fields, Field{Key: "Secret Name", Value: secret})
		}
		dns, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "dnsNames")
		if len(dns) > 0 {
			fields = append(fields, Field{Key: "DNS Names", Value: strings.Join(dns, ", ")})
		}
	}
	v.Sections = append(v.Sections, Section{Title: "Spec", Fields: fields})
	v.Conditions = certManagerConditions(*obj)
	if namespace != "" {
		v.Events = c.eventsFor(namespace, name)
	}
	return v, nil
}

func certManagerConditions(obj unstructured.Unstructured) []Condition {
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return nil
	}
	out := make([]Condition, 0, len(conds))
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		s, _, _ := unstructured.NestedString(m, "status")
		r, _, _ := unstructured.NestedString(m, "reason")
		msg, _, _ := unstructured.NestedString(m, "message")
		out = append(out, Condition{Type: t, Status: s, Reason: r, Message: msg})
	}
	return out
}

func (c *Client) deleteCertManagerResource(cx context.Context, namespace, resource, name string) error {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: resource}
	return dyn.Resource(gvr).Namespace(namespace).Delete(cx, name, metav1.DeleteOptions{})
}

func (c *Client) deleteCertManagerClusterResource(cx context.Context, resource, name string) error {
	dyn, err := dynamic.NewForConfig(c.cfg)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: resource}
	return dyn.Resource(gvr).Delete(cx, name, metav1.DeleteOptions{})
}

func (c *Client) helmReleaseDetail(cx context.Context, namespace, name string) (*DetailView, error) {
	secrets, err := c.cs.CoreV1().Secrets(namespace).List(cx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var rows [][]string
	status := "Unknown"
	var created metav1.Time
	for _, s := range secrets.Items {
		if s.Labels["owner"] != "helm" && !strings.HasPrefix(string(s.Type), "helm.sh/release.v1") {
			continue
		}
		relName := s.Labels["name"]
		if relName == "" {
			relName = helmNameFromSecret(s.Name)
		}
		if relName != name {
			continue
		}
		rev := s.Labels["version"]
		st := strings.Title(strings.ToLower(s.Labels["status"]))
		if st != "" {
			status = st
		}
		if created.IsZero() || s.CreationTimestamp.Time.Before(created.Time) {
			created = s.CreationTimestamp
		}
		rows = append(rows, []string{rev, st, s.Name, age(s.CreationTimestamp)})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("helm release %q not found in namespace %q", name, namespace)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i][0] > rows[j][0]
	})
	v := &DetailView{
		Kind:      "HelmRelease",
		Name:      name,
		Namespace: namespace,
		Status:    status,
		Age:       age(created),
		Sections: []Section{{
			Title: "Overview",
			Fields: []Field{
				{Key: "Release", Value: name},
				{Key: "Namespace", Value: namespace},
				{Key: "Status", Value: status},
			},
		}},
		Tables: []Table{{
			Title:   "Revision History (from Helm storage secrets)",
			Headers: []string{"Revision", "Status", "Storage Secret", "Age"},
			Rows:    rows,
		}},
	}
	return v, nil
}

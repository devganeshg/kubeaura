package k8s

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessCheck is one "can I <verb> <resource>?" question for
// SelfSubjectAccessReview. Group/Namespace/Name are optional.
type AccessCheck struct {
	Verb      string `json:"verb"`
	Group     string `json:"group"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// AccessResult echoes a check with the API server's decision.
type AccessResult struct {
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason"`
}

// CanI runs a batch of SelfSubjectAccessReviews against the current credential —
// the same "kubectl auth can-i" question. The UI uses the results to hide or
// disable actions the operator isn't allowed to perform (dynamic RBAC masking).
func (c *Client) CanI(checks []AccessCheck) ([]AccessResult, error) {
	cx, cancel := ctx()
	defer cancel()
	out := make([]AccessResult, 0, len(checks))
	for _, chk := range checks {
		sar := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace: chk.Namespace,
					Verb:      chk.Verb,
					Group:     chk.Group,
					Resource:  chk.Resource,
					Name:      chk.Name,
				},
			},
		}
		res, err := c.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(cx, sar, metav1.CreateOptions{})
		if err != nil {
			// Treat an error as "unknown -> not allowed" but keep the reason.
			out = append(out, AccessResult{Verb: chk.Verb, Resource: chk.Resource, Allowed: false, Reason: err.Error()})
			continue
		}
		out = append(out, AccessResult{
			Verb: chk.Verb, Resource: chk.Resource,
			Allowed: res.Status.Allowed, Reason: res.Status.Reason,
		})
	}
	return out, nil
}

// listRBAC extends the generic List switch with the RBAC kinds so they show up
// in the resource browser like any other kind.
func (c *Client) listRBAC(cx context.Context, kind, ns string, opts metav1.ListOptions) (ListResult, bool, error) {
	switch strings.ToLower(kind) {
	case "roles", "role":
		list, err := c.cs.RbacV1().Roles(ns).List(cx, opts)
		if err != nil {
			return ListResult{}, true, err
		}
		out := make([]Resource, 0, len(list.Items))
		for _, r := range list.Items {
			out = append(out, Resource{Kind: "Role", Name: r.Name, Namespace: r.Namespace, Status: "—",
				Info: fmt.Sprintf("%d rules", len(r.Rules)), Age: age(r.CreationTimestamp), Labels: r.Labels})
		}
		return ListResult{Items: out, Continue: list.Continue}, true, nil
	case "rolebindings", "rolebinding":
		list, err := c.cs.RbacV1().RoleBindings(ns).List(cx, opts)
		if err != nil {
			return ListResult{}, true, err
		}
		out := make([]Resource, 0, len(list.Items))
		for _, b := range list.Items {
			subs := make([]string, 0, len(b.Subjects))
			for _, s := range b.Subjects {
				subs = append(subs, s.Kind+":"+s.Name)
			}
			out = append(out, Resource{Kind: "RoleBinding", Name: b.Name, Namespace: b.Namespace, Status: b.RoleRef.Kind + "/" + b.RoleRef.Name,
				Info: strings.Join(subs, ", "), Age: age(b.CreationTimestamp), Labels: b.Labels})
		}
		return ListResult{Items: out, Continue: list.Continue}, true, nil
	case "clusterroles", "clusterrole":
		list, err := c.cs.RbacV1().ClusterRoles().List(cx, opts)
		if err != nil {
			return ListResult{}, true, err
		}
		out := make([]Resource, 0, len(list.Items))
		for _, r := range list.Items {
			out = append(out, Resource{Kind: "ClusterRole", Name: r.Name, Status: "—",
				Info: fmt.Sprintf("%d rules", len(r.Rules)), Age: age(r.CreationTimestamp), Labels: r.Labels})
		}
		return ListResult{Items: out, Continue: list.Continue}, true, nil
	case "clusterrolebindings", "clusterrolebinding":
		list, err := c.cs.RbacV1().ClusterRoleBindings().List(cx, opts)
		if err != nil {
			return ListResult{}, true, err
		}
		out := make([]Resource, 0, len(list.Items))
		for _, b := range list.Items {
			out = append(out, Resource{Kind: "ClusterRoleBinding", Name: b.Name, Status: b.RoleRef.Kind + "/" + b.RoleRef.Name,
				Info: fmt.Sprintf("%d subjects", len(b.Subjects)), Age: age(b.CreationTimestamp), Labels: b.Labels})
		}
		return ListResult{Items: out, Continue: list.Continue}, true, nil
	case "serviceaccounts", "serviceaccount", "sa":
		list, err := c.cs.CoreV1().ServiceAccounts(ns).List(cx, opts)
		if err != nil {
			return ListResult{}, true, err
		}
		out := make([]Resource, 0, len(list.Items))
		for _, s := range list.Items {
			out = append(out, Resource{Kind: "ServiceAccount", Name: s.Name, Namespace: s.Namespace, Status: "—",
				Info: fmt.Sprintf("%d secrets", len(s.Secrets)), Age: age(s.CreationTimestamp), Labels: s.Labels})
		}
		return ListResult{Items: out, Continue: list.Continue}, true, nil
	}
	return ListResult{}, false, nil
}

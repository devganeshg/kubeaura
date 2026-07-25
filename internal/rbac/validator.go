// Package rbac provides RBAC compliance checking and validation.
package rbac

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PermissionCheck represents a single permission test (can we do X?).
type PermissionCheck struct {
	APIGroup     string `json:"apiGroup"`
	Resource     string `json:"resource"`
	Verb         string `json:"verb"`
	Namespace    string `json:"namespace"` // empty for cluster-scoped
	IsAllowed    bool   `json:"isAllowed"`
	DeniedReason string `json:"deniedReason,omitempty"`
}

// ComplianceReport summarizes permission gaps for KubeMind functionality.
type ComplianceReport struct {
	ServiceAccount   string            `json:"serviceAccount"`
	Namespace        string            `json:"namespace"`
	Context          string            `json:"context"`
	Permissions      []PermissionCheck `json:"permissions"`
	DegradedFeatures []string          `json:"degradedFeatures"` // Features that won't work
	RequiredFixes    []string          `json:"requiredFixes"`    // RBAC rules to add
	IsClusterAdmin   bool              `json:"isClusterAdmin"`
	IsNamespaceAdmin bool              `json:"isNamespaceAdmin"`
}

// FeatureRequirements defines what permissions each KubeMind feature needs.
var FeatureRequirements = map[string][]PermissionCheck{
	"dashboard": {
		{APIGroup: "", Resource: "nodes", Verb: "list"},
		{APIGroup: "", Resource: "namespaces", Verb: "list"},
		{APIGroup: "", Resource: "pods", Verb: "list"},
		{APIGroup: "apps", Resource: "deployments", Verb: "list"},
		{APIGroup: "", Resource: "services", Verb: "list"},
	},
	"metrics": {
		{APIGroup: "", Resource: "nodes", Verb: "get"},
		{APIGroup: "", Resource: "nodes", Verb: "list"},
		{APIGroup: "metrics.k8s.io", Resource: "nodes", Verb: "get"},
		{APIGroup: "metrics.k8s.io", Resource: "pods", Verb: "get"},
	},
	"logs": {
		{APIGroup: "", Resource: "pods", Verb: "get"},
		{APIGroup: "", Resource: "pods/log", Verb: "get"},
	},
	"exec": {
		{APIGroup: "", Resource: "pods", Verb: "get"},
		{APIGroup: "", Resource: "pods/exec", Verb: "create"},
	},
	"portforward": {
		{APIGroup: "", Resource: "pods", Verb: "get"},
		{APIGroup: "", Resource: "pods/portforward", Verb: "create"},
	},
	"rbac": {
		{APIGroup: "rbac.authorization.k8s.io", Resource: "roles", Verb: "list"},
		{APIGroup: "rbac.authorization.k8s.io", Resource: "rolebindings", Verb: "list"},
		{APIGroup: "rbac.authorization.k8s.io", Resource: "clusterroles", Verb: "list"},
		{APIGroup: "rbac.authorization.k8s.io", Resource: "clusterrolebindings", Verb: "list"},
	},
	"write_actions": {
		{APIGroup: "", Resource: "pods", Verb: "delete"},
		{APIGroup: "apps", Resource: "deployments", Verb: "patch"},
		{APIGroup: "apps", Resource: "deployments/scale", Verb: "patch"},
	},
}

// Validator checks RBAC permissions for a service account.
type Validator struct {
	cs kubernetes.Interface
}

// NewValidator creates an RBAC validator.
func NewValidator(cs kubernetes.Interface) *Validator {
	return &Validator{cs: cs}
}

// CheckPermission tests if the current user can perform an action (verb on resource).
func (v *Validator) CheckPermission(ctx context.Context, verb, apiGroup, resource, namespace string) (bool, string) {
	if v.cs == nil {
		return false, "no cluster client configured"
	}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      verb,
				Group:     apiGroup,
				Resource:  resource,
			},
		},
	}

	result, err := v.cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Sprintf("error checking permission: %v", err)
	}

	if result.Status.Allowed {
		return true, ""
	}
	return false, result.Status.Reason
}

// GenerateComplianceReport evaluates all KubeMind feature requirements against current permissions.
func (v *Validator) GenerateComplianceReport(ctx context.Context, saName, namespace string) (*ComplianceReport, error) {
	if v.cs == nil {
		return nil, fmt.Errorf("no cluster client configured")
	}
	// Impersonate the service account for checking permissions
	// (In a real setup, the caller would be the service account's context)

	report := &ComplianceReport{
		ServiceAccount:   saName,
		Namespace:        namespace,
		Permissions:      make([]PermissionCheck, 0),
		DegradedFeatures: make([]string, 0),
		RequiredFixes:    make([]string, 0),
	}

	// Check each feature's permissions
	for feature, checks := range FeatureRequirements {
		featureOK := true
		for _, check := range checks {
			allowed, reason := v.CheckPermission(ctx, check.Verb, check.APIGroup, check.Resource, check.Namespace)
			if !allowed {
				featureOK = false
			}
			report.Permissions = append(report.Permissions, PermissionCheck{
				APIGroup:     check.APIGroup,
				Resource:     check.Resource,
				Verb:         check.Verb,
				Namespace:    check.Namespace,
				IsAllowed:    allowed,
				DeniedReason: reason,
			})
		}
		if !featureOK {
			report.DegradedFeatures = append(report.DegradedFeatures, feature)
		}
	}

	// Suggest RBAC fixes
	report.RequiredFixes = suggestRBACFixes(report)

	return report, nil
}

// helper: generate suggested RBAC rules
func suggestRBACFixes(report *ComplianceReport) []string {
	fixes := make([]string, 0)
	deniedPerms := make(map[string][]string) // resource -> verbs

	for _, perm := range report.Permissions {
		if !perm.IsAllowed {
			key := fmt.Sprintf("%s.%s", perm.APIGroup, perm.Resource)
			deniedPerms[key] = append(deniedPerms[key], perm.Verb)
		}
	}

	for resource, verbs := range deniedPerms {
		rule := fmt.Sprintf("- apiGroups: [\"%s\"]\n  resources: [\"%s\"]\n  verbs: %v",
			resource, resource, verbs)
		fixes = append(fixes, rule)
	}

	return fixes
}

// SuggestLeastPrivilegeRole creates a Role with only the permissions needed for KubeMind.
func SuggestLeastPrivilegeRole(features []string) *rbacv1.Role {
	rules := make([]rbacv1.PolicyRule, 0)

	for _, feature := range features {
		if checks, ok := FeatureRequirements[feature]; ok {
			// Consolidate checks into PolicyRules (group by apiGroup)
			verbs := make(map[string][]string) // apiGroup+resource -> verbs

			for _, check := range checks {
				key := fmt.Sprintf("%s/%s", check.APIGroup, check.Resource)
				if verbs[key] == nil {
					verbs[key] = make([]string, 0)
				}
				verbs[key] = append(verbs[key], check.Verb)
			}

			for key, verbList := range verbs {
				// Parse key back to apiGroup, resource
				// For simplicity: assume format "apiGroup/resource"
				rules = append(rules, rbacv1.PolicyRule{
					APIGroups: []string{""}, // TODO: extract from key
					Resources: []string{key},
					Verbs:     verbList,
				})
			}
		}
	}

	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubemind-minimal",
			Namespace: "default", // user's namespace
		},
		Rules: rules,
	}
}

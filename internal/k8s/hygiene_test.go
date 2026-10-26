package k8s

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHygieneFindsReviewCandidates(t *testing.T) {
	zero, one := int32(0), int32(1)
	client := fakeClusterClient("test",
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "mounted-pod", Namespace: "prod", Labels: map[string]string{"app": "web"}},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "in-use"}}}}},
		},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "orphan-service", Namespace: "prod"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "missing"}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "active-service", Namespace: "prod"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "selectorless", Namespace: "prod"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "prod"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "in-use", Namespace: "prod"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: "prod"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "paused", Namespace: "prod"}, Spec: appsv1.DeploymentSpec{Replicas: &zero}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "prod"}, Spec: appsv1.DeploymentSpec{Replicas: &one}},
	)

	report, err := client.Hygiene("prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(report.Findings), report.Findings)
	}
	want := map[string]string{
		"Service/orphan-service": "Selector matches no pods",
		"PVC/unused":             "Not mounted by any pod",
		"Deployment/paused":      "Desired replicas set to zero",
	}
	for _, finding := range report.Findings {
		key := finding.Kind + "/" + finding.Name
		if finding.Namespace != "prod" || finding.Reason != want[key] {
			t.Errorf("unexpected finding: %+v", finding)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Errorf("missing findings: %v", want)
	}
}
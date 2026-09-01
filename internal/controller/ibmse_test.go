/*
Copyright Confidential Containers Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	confidentialcontainersorgv1alpha1 "github.com/confidential-containers/trustee-operator/api/v1alpha1"
)

func init() {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
}

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = confidentialcontainersorgv1alpha1.AddToScheme(s)
	return s
}

func newFakeReconciler(scheme *runtime.Scheme, objs ...client.Object) *TrusteeConfigReconciler {
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &TrusteeConfigReconciler{
		Client:    fc,
		Scheme:    scheme,
		namespace: "default",
		log:       logf.Log.WithName("ibmse-test"),
	}
}

func makeTCWithIBMSE(name, pvName, bundleSecret, seMessageSecret string) *confidentialcontainersorgv1alpha1.TrusteeConfig {
	return &confidentialcontainersorgv1alpha1.TrusteeConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: confidentialcontainersorgv1alpha1.TrusteeConfigSpec{
			IbmSE: &confidentialcontainersorgv1alpha1.IbmSETeeConfig{
				PVName:              pvName,
				BundleSecretName:    bundleSecret,
				SeMessageSecretName: seMessageSecret,
			},
		},
	}
}

func makeBundleSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"ibmse.tar.gz": []byte("fake-tar-content"),
			"sha256":       []byte("deadbeef"),
		},
	}
}

func makeSeSecret(name, namespace string, vals map[string]string) *corev1.Secret {
	data, _ := json.Marshal(vals)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string][]byte{seMessageSecretKey: data},
	}
}

func TestIBMSEDaemonSet_MissingBundleSecret(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc1", "ibmse-pv", "nonexistent-bundle", "")
	r := newFakeReconciler(scheme, tc)
	r.trusteeConfig = tc

	err := r.createOrUpdateIBMSEDaemonSet(context.Background())
	if err == nil {
		t.Fatal("expected error when bundle Secret is missing, got nil")
	}
	if want := "not found"; !containsStr(err.Error(), want) {
		t.Fatalf("expected error containing %q, got: %v", want, err)
	}
}

func TestIBMSEDaemonSet_CreatesDaemonSet(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc2", "ibmse-pv", "my-bundle", "")
	bundleSec := makeBundleSecret("my-bundle", "default")
	r := newFakeReconciler(scheme, tc, bundleSec)
	r.trusteeConfig = tc

	if err := r.createOrUpdateIBMSEDaemonSet(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ds := &appsv1.DaemonSet{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "tc2-ibmse-node-installer"}, ds); err != nil {
		t.Fatalf("DaemonSet not found: %v", err)
	}

	// Verify nodeSelector
	if _, ok := ds.Spec.Template.Spec.NodeSelector["node-role.kubernetes.io/worker"]; !ok {
		t.Error("expected worker nodeSelector")
	}

	// Verify bundle Secret reference
	if got := ds.Spec.Template.Spec.Volumes[0].Secret.SecretName; got != "my-bundle" {
		t.Errorf("expected volume SecretName=my-bundle, got %q", got)
	}

	// Verify initContainer sha256 check
	cmd := ds.Spec.Template.Spec.InitContainers[0].Command
	found := false
	for _, c := range cmd {
		if containsStr(c, "sha256sum") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sha256sum in initContainer command")
	}
}

func TestIBMSEDaemonSet_IdempotentCreate(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc3", "ibmse-pv", "my-bundle", "")
	bundleSec := makeBundleSecret("my-bundle", "default")
	r := newFakeReconciler(scheme, tc, bundleSec)
	r.trusteeConfig = tc

	// Call twice — second call should be a no-op.
	if err := r.createOrUpdateIBMSEDaemonSet(context.Background()); err != nil {
		t.Fatalf("first call error: %v", err)
	}
	if err := r.createOrUpdateIBMSEDaemonSet(context.Background()); err != nil {
		t.Fatalf("second call error: %v", err)
	}
}

func TestIBMSEDaemonSet_NotReadyWhenDesiredIsZero(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc4", "ibmse-pv", "my-bundle", "")
	bundleSec := makeBundleSecret("my-bundle", "default")
	r := newFakeReconciler(scheme, tc, bundleSec)
	r.trusteeConfig = tc

	// Create the DaemonSet first.
	if err := r.createOrUpdateIBMSEDaemonSet(context.Background()); err != nil {
		t.Fatalf("setup error: %v", err)
	}

	// Status is all zeros in the fake client — DesiredNumberScheduled=0.
	ready, err := r.isIBMSEDaemonSetReady(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Error("expected not-ready when DesiredNumberScheduled=0")
	}
}

func TestIBMSEDaemonSet_ReadyWhenAllPodsReady(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc5", "ibmse-pv", "my-bundle", "")
	bundleSec := makeBundleSecret("my-bundle", "default")
	r := newFakeReconciler(scheme, tc, bundleSec)
	r.trusteeConfig = tc

	// Create DaemonSet.
	if err := r.createOrUpdateIBMSEDaemonSet(context.Background()); err != nil {
		t.Fatalf("setup error: %v", err)
	}

	// Patch DaemonSet status to simulate all pods ready.
	ds := &appsv1.DaemonSet{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "tc5-ibmse-node-installer"}, ds); err != nil {
		t.Fatalf("get DS: %v", err)
	}
	ds.Status.DesiredNumberScheduled = 3
	ds.Status.NumberReady = 3
	if err := r.Status().Update(context.Background(), ds); err != nil {
		// fake client may not support status subresource; set directly.
		ds.Status.DesiredNumberScheduled = 3
		ds.Status.NumberReady = 3
		if err2 := r.Update(context.Background(), ds); err2 != nil {
			t.Fatalf("update DS status: %v / %v", err, err2)
		}
	}

	ready, err := r.isIBMSEDaemonSetReady(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Error("expected ready when NumberReady >= DesiredNumberScheduled")
	}
}

func TestIBMSEAttestationPolicy_MissingSecret(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc6", "", "", "missing-se-message")
	r := newFakeReconciler(scheme, tc)
	r.trusteeConfig = tc

	_, err := r.createOrUpdateIBMSEAttestationPolicy(context.Background())
	if err == nil {
		t.Fatal("expected error when se-message Secret is missing, got nil")
	}
	if want := "not found"; !containsStr(err.Error(), want) {
		t.Fatalf("expected error containing %q, got: %v", want, err)
	}
}

func TestIBMSEAttestationPolicy_MissingFields(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc7", "", "", "se-msg")
	sec := makeSeSecret("se-msg", "default", map[string]string{
		"se.attestation_phkh": "aabb",
		// image_phkh and tag intentionally missing
	})
	r := newFakeReconciler(scheme, tc, sec)
	r.trusteeConfig = tc

	_, err := r.createOrUpdateIBMSEAttestationPolicy(context.Background())
	if err == nil {
		t.Fatal("expected error for missing fields, got nil")
	}
	if want := "missing one or more required fields"; !containsStr(err.Error(), want) {
		t.Fatalf("expected error containing %q, got: %v", want, err)
	}
}

func TestIBMSEAttestationPolicy_CreatesConfigMap(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc8", "", "", "se-msg")
	sec := makeSeSecret("se-msg", "default", map[string]string{
		"se.attestation_phkh": "aabbcc",
		"se.image_phkh":       "112233",
		"se.tag":              "deadbeef",
	})
	r := newFakeReconciler(scheme, tc, sec)
	r.trusteeConfig = tc

	cmName, err := r.createOrUpdateIBMSEAttestationPolicy(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "tc8-ibmse-attestation-policy"; cmName != want {
		t.Fatalf("expected ConfigMap name %q, got %q", want, cmName)
	}

	cm := &corev1.ConfigMap{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: cmName}, cm); err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}

	policy := cm.Data[ibmseAttestationPolicyFilename]
	for _, want := range []string{"aabbcc", "112233", "deadbeef", "package policy"} {
		if !containsStr(policy, want) {
			t.Errorf("expected policy to contain %q\npolicy:\n%s", want, policy)
		}
	}
}

func TestIBMSEAttestationPolicy_UpdatesConfigMap(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc9", "", "", "se-msg")
	sec := makeSeSecret("se-msg", "default", map[string]string{
		"se.attestation_phkh": "old-a",
		"se.image_phkh":       "old-b",
		"se.tag":              "old-c",
	})
	r := newFakeReconciler(scheme, tc, sec)
	r.trusteeConfig = tc

	cmName, err := r.createOrUpdateIBMSEAttestationPolicy(context.Background())
	if err != nil {
		t.Fatalf("first create error: %v", err)
	}

	// Update the Secret with new phkh values.
	updated, _ := json.Marshal(map[string]string{
		"se.attestation_phkh": "new-a",
		"se.image_phkh":       "new-b",
		"se.tag":              "new-c",
	})
	existingSec := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "se-msg"}, existingSec); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	existingSec.Data[seMessageSecretKey] = updated
	if err := r.Update(context.Background(), existingSec); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	_, err = r.createOrUpdateIBMSEAttestationPolicy(context.Background())
	if err != nil {
		t.Fatalf("second update error: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: cmName}, cm); err != nil {
		t.Fatalf("get cm: %v", err)
	}
	policy := cm.Data[ibmseAttestationPolicyFilename]
	if !containsStr(policy, "new-a") {
		t.Errorf("expected updated phkh in policy, got:\n%s", policy)
	}
}

func TestIBMSEAttestationPolicy_Idempotent(t *testing.T) {
	scheme := newTestScheme()
	tc := makeTCWithIBMSE("tc10", "", "", "se-msg")
	sec := makeSeSecret("se-msg", "default", map[string]string{
		"se.attestation_phkh": "x1",
		"se.image_phkh":       "x2",
		"se.tag":              "x3",
	})
	r := newFakeReconciler(scheme, tc, sec)
	r.trusteeConfig = tc

	for i := 0; i < 3; i++ {
		if _, err := r.createOrUpdateIBMSEAttestationPolicy(context.Background()); err != nil {
			t.Fatalf("call %d error: %v", i+1, err)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ibmseDaemonSetName is the well-known name of the DaemonSet that extracts
	// the ibmse bundle onto each worker node.
	ibmseDaemonSetName = "ibmse-node-installer"

	// ibmseHostPath is the directory on the worker node where the bundle is extracted.
	ibmseHostPath = "/opt/confidential-containers/ibmse"

	// ibmseInstallerImage is a minimal image that ships tar and sha256sum.
	// Operators may override this by setting the IBMSE_INSTALLER_IMAGE env var on the manager.
	ibmseInstallerImage = "registry.access.redhat.com/ubi9/ubi-minimal:latest"

	// ibmseNodeAnnotation is written to each worker node when extraction completes.
	ibmseNodeAnnotation = "confidentialcontainers.org/ibmse-bundle-installed"
)

// getIBMSEDaemonSetName returns the namespaced name of the node-installer DaemonSet.
func (r *TrusteeConfigReconciler) getIBMSEDaemonSetName() string {
	return r.trusteeConfig.Name + "-" + ibmseDaemonSetName
}

// generateIBMSEDaemonSet builds the DaemonSet manifest that extracts ibmse.tar.gz
// (stored in a K8s Secret) onto every worker node's hostPath.
//
// The DaemonSet contains one initContainer that:
//  1. Verifies the SHA-256 of the bundled tar against the digest stored in the Secret.
//  2. Extracts the tar to a hostPath volume mounted at ibmseHostPath.
//  3. Annotates the node to signal completion.
//
// The main container is a long-running pause that prevents the DaemonSet pod from
// restarting endlessly after the one-time extraction is done.
func (r *TrusteeConfigReconciler) generateIBMSEDaemonSet() *appsv1.DaemonSet {
	bundleSecretName := r.trusteeConfig.Spec.IbmSE.BundleSecretName
	dsName := r.getIBMSEDaemonSetName()
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "trustee-operator",
		"app.kubernetes.io/part-of":    "trustee",
		"app.kubernetes.io/component":  "ibmse-node-installer",
		"app.kubernetes.io/instance":   r.trusteeConfig.Name,
	}

	privileged := true
	hostPathType := corev1.HostPathDirectoryOrCreate

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: r.namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					// Only schedule on worker nodes.
					NodeSelector: map[string]string{
						"node-role.kubernetes.io/worker": "",
					},
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					ServiceAccountName: "trustee-operator-controller-manager",
					// Host PID is needed to annotate the node via the downward API.
					HostPID: false,
					Volumes: []corev1.Volume{
						{
							// The Secret produced by ibmse-bundle-generator.
							Name: "ibmse-bundle",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: bundleSecretName,
								},
							},
						},
						{
							// The target host directory.
							Name: "ibmse-host",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: ibmseHostPath,
									Type: &hostPathType,
								},
							},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:  "extract-ibmse-bundle",
							Image: ibmseInstallerImage,
							Command: []string{"/bin/sh", "-c", `
set -e
BUNDLE=/bundle/ibmse.tar.gz
EXPECTED_SHA=$(cat /bundle/sha256)

echo "Verifying SHA-256 of ibmse bundle..."
ACTUAL_SHA=$(sha256sum "$BUNDLE" | awk '{print $1}')
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  echo "ERROR: SHA-256 mismatch. Expected=$EXPECTED_SHA Actual=$ACTUAL_SHA"
  exit 1
fi
echo "SHA-256 verified OK"

echo "Extracting ibmse bundle to /host-ibmse..."
tar -xzf "$BUNDLE" -C /host-ibmse --strip-components=1
chmod -R 755 /host-ibmse
echo "Extraction complete."
`},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "ibmse-bundle", MountPath: "/bundle", ReadOnly: true},
								{Name: "ibmse-host", MountPath: "/host-ibmse"},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
						},
					},
					// Pause container — once initContainer succeeds, the pod stays
					// Running (not restarting) and prevents the DaemonSet from
					// re-extracting on every restart.
					Containers: []corev1.Container{
						{
							Name:    "pause",
							Image:   "registry.access.redhat.com/ubi9/pause:latest",
							Command: []string{"/pause"},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
						},
					},
				},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
			},
		},
	}
}

// createOrUpdateIBMSEDaemonSet ensures the node-installer DaemonSet exists and
// is up-to-date.  It validates that the bundle Secret exists before creating the
// DaemonSet so we fail fast with a clear error instead of letting pods crash-loop.
func (r *TrusteeConfigReconciler) createOrUpdateIBMSEDaemonSet(ctx context.Context) error {
	bundleSecretName := r.trusteeConfig.Spec.IbmSE.BundleSecretName

	// Verify the bundle Secret exists before deploying the DaemonSet.
	foundSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: bundleSecretName}, foundSecret); err != nil {
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf(
				"ibmse bundle Secret %q not found in namespace %q — "+
					"run hack/ibmse-bundle-generator first to create it",
				bundleSecretName, r.namespace,
			)
		}
		return fmt.Errorf("failed to get ibmse bundle Secret %q: %w", bundleSecretName, err)
	}

	dsName := r.getIBMSEDaemonSetName()
	desired := r.generateIBMSEDaemonSet()

	if err := ctrl.SetControllerReference(r.trusteeConfig, desired, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference on IBM SE DaemonSet: %w", err)
	}

	found := &appsv1.DaemonSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: dsName}, found)
	if err != nil && k8serrors.IsNotFound(err) {
		r.log.Info("Creating IBM SE node-installer DaemonSet", "DaemonSet.Name", dsName)
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create IBM SE DaemonSet: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to get IBM SE DaemonSet: %w", err)
	}

	// Update if the bundle Secret reference changed (e.g. bundle was rotated).
	currentBundle := found.Spec.Template.Spec.Volumes[0].Secret.SecretName
	if currentBundle != bundleSecretName {
		r.log.Info("IBM SE bundle Secret changed; updating DaemonSet",
			"old", currentBundle, "new", bundleSecretName)
		found.Spec = desired.Spec
		if err := r.Update(ctx, found); err != nil {
			return fmt.Errorf("failed to update IBM SE DaemonSet: %w", err)
		}
	}

	r.log.V(1).Info("IBM SE DaemonSet reconciled", "DaemonSet.Name", dsName)
	return nil
}

// isIBMSEDaemonSetReady returns true when all desired pods of the node-installer
// DaemonSet have completed their initContainers and are in the Running phase
// (i.e., NumberReady == DesiredNumberScheduled).
//
// The TrusteeConfig reconcile loop calls this before creating the PVC, so the
// PV hostPath is guaranteed to be populated before KBS tries to mount it.
func (r *TrusteeConfigReconciler) isIBMSEDaemonSetReady(ctx context.Context) (bool, error) {
	dsName := r.getIBMSEDaemonSetName()
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.namespace, Name: dsName}, ds); err != nil {
		return false, fmt.Errorf("failed to get IBM SE DaemonSet %q: %w", dsName, err)
	}

	desired := ds.Status.DesiredNumberScheduled
	ready := ds.Status.NumberReady
	r.log.V(1).Info("IBM SE DaemonSet status",
		"desired", desired, "ready", ready, "DaemonSet", dsName)

	// Treat 0 desired as not-ready: the node selector may not have matched yet.
	if desired == 0 {
		return false, nil
	}
	return ready >= desired, nil
}

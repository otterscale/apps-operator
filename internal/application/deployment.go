/*
Copyright 2026 The OtterScale Authors.

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

package application

import (
	"context"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
)

// ReconcileDeployment ensures the Deployment exists and matches the desired state
// declared in Application.Spec.Deployment.
func ReconcileDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *workloadv1alpha1.Application, version string) error {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := ctrlutil.CreateOrPatch(ctx, c, deploy, func() error {
		// Should not happen due to CEL validation, but guard against panic from
		if app.Spec.DeploymentConfig == nil {
			return fmt.Errorf("application %s/%s has workloadType Deployment but spec.deploymentConfig is nil",
				app.Namespace, app.Name)
		}
		deploy.Spec = app.Spec.DeploymentConfig.Deployment

		// If a PVC is requested, inject the corresponding Volume and VolumeMounts
		// into the pod spec so containers can access the persistent storage.
		if app.Spec.DeploymentConfig.PersistentVolumeClaim != nil {
			injectPVC(&deploy.Spec, app.Name, app.Spec.DeploymentConfig.MountPath)
		}

		if deploy.Labels == nil {
			deploy.Labels = map[string]string{}
		}
		maps.Copy(deploy.Labels, LabelsForApplication(app.Name, version))

		return ctrlutil.SetControllerReference(app, deploy, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("Deployment reconciled", "operation", op, "name", deploy.Name)
	}
	return nil
}

// injectPVC ensures the PVC-backed Volume and per-container VolumeMounts are
// present in the Deployment pod spec. It is idempotent: if an entry with the
// given name already exists it is updated in place rather than duplicated.
// The volume name equals the PVC name (which equals app.Name).
func injectPVC(spec *appsv1.DeploymentSpec, pvcName, mountPath string) {
	// Ensure the Volume entry exists and points to the correct PVC.
	volumeFound := false
	for i, v := range spec.Template.Spec.Volumes {
		if v.Name == pvcName {
			spec.Template.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			}
			volumeFound = true
			break
		}
	}
	if !volumeFound {
		spec.Template.Spec.Volumes = append(spec.Template.Spec.Volumes, corev1.Volume{
			Name: pvcName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		})
	}

	// Ensure every container has a VolumeMount for this volume.
	for i := range spec.Template.Spec.Containers {
		mountFound := false
		for j, vm := range spec.Template.Spec.Containers[i].VolumeMounts {
			if vm.Name == pvcName {
				spec.Template.Spec.Containers[i].VolumeMounts[j].MountPath = mountPath
				mountFound = true
				break
			}
		}
		if !mountFound {
			spec.Template.Spec.Containers[i].VolumeMounts = append(
				spec.Template.Spec.Containers[i].VolumeMounts,
				corev1.VolumeMount{
					Name:      pvcName,
					MountPath: mountPath,
				},
			)
		}
	}
}

// CleanupDeployment deletes the Deployment owned by the Application if it exists.
// This is called when transitioning from Deployment mode to CronJob mode.
// It verifies the OwnerReference before deleting to avoid removing resources
// not owned by this Application.
func CleanupDeployment(ctx context.Context, c client.Client, app *workloadv1alpha1.Application) error {
	var deploy appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &deploy); err != nil {
		return client.IgnoreNotFound(err)
	}
	for _, ref := range deploy.OwnerReferences {
		if ref.UID == app.UID {
			if err := c.Delete(ctx, &deploy); client.IgnoreNotFound(err) != nil {
				return err
			}
			log.FromContext(ctx).Info("Deployment cleaned up", "name", deploy.Name)
			return nil
		}
	}
	log.FromContext(ctx).Info("Deployment not owned by this Application, skipping cleanup", "name", deploy.Name)
	return nil
}

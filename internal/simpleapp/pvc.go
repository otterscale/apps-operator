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

package simpleapp

import (
	"context"
	"encoding/json"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/otterscale/api/apps/v1alpha1"
)

// ReconcilePVC ensures the PersistentVolumeClaim matches the desired state or
// is deleted when the spec is removed.
//
// Most PVC spec fields are immutable after the claim is bound. The only mutable
// field is spec.resources.requests.storage, which supports volume expansion when
// the StorageClass has allowVolumeExpansion: true. On update, we synchronize the
// storage request along with labels and owner references.
func ReconcilePVC(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *appsv1alpha1.SimpleApp, version string) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	if app.Spec.PersistentVolumeClaim == nil {
		return client.IgnoreNotFound(c.Delete(ctx, pvc))
	}

	var pvcSpec corev1.PersistentVolumeClaimSpec
	if err := json.Unmarshal(app.Spec.PersistentVolumeClaim.Raw, &pvcSpec); err != nil {
		return &InvalidSpecError{Field: "persistentVolumeClaim", Message: err.Error()}
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, c, pvc, func() error {
		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec = pvcSpec
		} else {
			// Volume expansion: update storage request if the desired size is larger.
			// Kubernetes only allows increasing the size; the API server enforces this.
			if desired, ok := pvcSpec.Resources.Requests[corev1.ResourceStorage]; ok {
				if pvc.Spec.Resources.Requests == nil {
					pvc.Spec.Resources.Requests = corev1.ResourceList{}
				}
				pvc.Spec.Resources.Requests[corev1.ResourceStorage] = desired
			}
		}

		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		maps.Copy(pvc.Labels, LabelsForSimpleApp(app.Name, version))

		return ctrlutil.SetControllerReference(app, pvc, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("PersistentVolumeClaim reconciled", "operation", op, "name", pvc.Name)
	}
	return nil
}

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
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
)

// ReconcileService ensures the Service matches the desired state or is deleted
// when the spec is removed.
func ReconcileService(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *workloadv1alpha1.Application, version string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	if app.Spec.DeploymentConfig == nil || app.Spec.DeploymentConfig.Service == nil {
		return client.IgnoreNotFound(c.Delete(ctx, svc))
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, c, svc, func() error {
		// Preserve ClusterIP on update — Kubernetes treats it as immutable once assigned.
		clusterIP := svc.Spec.ClusterIP
		nodePorts := make(map[int32]int32, len(svc.Spec.Ports))
		for _, p := range svc.Spec.Ports {
			if p.NodePort != 0 {
				nodePorts[p.Port] = p.NodePort
			}
		}

		// DeepCopy before assigning to avoid mutating the Application's in-memory
		// ServiceSpec via shared slice/map backing arrays when restoring ClusterIP
		// and NodePort values below.
		svc.Spec = *app.Spec.DeploymentConfig.Service.DeepCopy()

		if clusterIP != "" {
			svc.Spec.ClusterIP = clusterIP
		}
		// Restore Kubernetes-assigned NodePort values so we don't trigger
		// unnecessary port re-assignment on every reconcile.
		for i := range svc.Spec.Ports {
			if svc.Spec.Ports[i].NodePort == 0 {
				if np, ok := nodePorts[svc.Spec.Ports[i].Port]; ok {
					svc.Spec.Ports[i].NodePort = np
				}
			}
		}

		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		maps.Copy(svc.Labels, LabelsForApplication(app.Name, version))

		return ctrlutil.SetControllerReference(app, svc, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("Service reconciled", "operation", op, "name", svc.Name)
	}
	return nil
}

// CleanupService deletes the Service owned by the Application if it exists.
// This is called when transitioning from Deployment mode to CronJob mode.
func CleanupService(ctx context.Context, c client.Client, app *workloadv1alpha1.Application) error {
	return CleanupOwnedResource(ctx, c, app, &corev1.Service{}, "Service")
}

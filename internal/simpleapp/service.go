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

// ReconcileService ensures the Service matches the desired state or is deleted
// when the spec is removed.
func ReconcileService(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *appsv1alpha1.SimpleApp, version string) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	if app.Spec.Service == nil {
		return client.IgnoreNotFound(c.Delete(ctx, svc))
	}

	var svcSpec corev1.ServiceSpec
	if err := json.Unmarshal(app.Spec.Service.Raw, &svcSpec); err != nil {
		return &InvalidSpecError{Field: "service", Message: err.Error()}
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, c, svc, func() error {
		// Preserve ClusterIP on update — Kubernetes treats it as immutable once assigned.
		clusterIP := svc.Spec.ClusterIP
		svc.Spec = svcSpec
		if clusterIP != "" {
			svc.Spec.ClusterIP = clusterIP
		}

		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		maps.Copy(svc.Labels, LabelsForSimpleApp(app.Name, version))

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

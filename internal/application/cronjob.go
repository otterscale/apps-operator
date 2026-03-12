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

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
)

// ReconcileCronJob ensures the CronJob exists and matches the desired state
// declared in Application.Spec.CronJob.
func ReconcileCronJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *workloadv1alpha1.Application, version string) error {
	cj := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := ctrlutil.CreateOrPatch(ctx, c, cj, func() error {
		if app.Spec.CronJob == nil {
			// Should not happen due to CEL validation, but guard against panic from
			return fmt.Errorf("application %s/%s has workloadType CronJob but spec.cronJob is nil",
				app.Namespace, app.Name)
		}
		cj.Spec = *app.Spec.CronJob

		if cj.Labels == nil {
			cj.Labels = map[string]string{}
		}
		maps.Copy(cj.Labels, LabelsForApplication(app.Name, version))

		return ctrlutil.SetControllerReference(app, cj, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("CronJob reconciled", "operation", op, "name", cj.Name)
	}
	return nil
}

// CleanupCronJob deletes the CronJob owned by the Application if it exists.
// This is called when transitioning from CronJob mode back to Deployment mode.
func CleanupCronJob(ctx context.Context, c client.Client, app *workloadv1alpha1.Application) error {
	return CleanupOwnedResource(ctx, c, app, &batchv1.CronJob{}, "CronJob")
}

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

// ReconcileJob ensures the Job exists and matches the desired state
// declared in Application.Spec.Job.
//
// Job spec is largely immutable after creation (Kubernetes rejects most field
// changes). Therefore the spec is only written on initial creation; subsequent
// reconcile loops only synchronize labels and the OwnerReference.
func ReconcileJob(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *workloadv1alpha1.Application, version string) error {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := ctrlutil.CreateOrPatch(ctx, c, job, func() error {
		// Should not happen due to CEL validation, but guard against panic.
		if app.Spec.Job == nil {
			return fmt.Errorf("application %s/%s has workloadType Job but spec.job is nil",
				app.Namespace, app.Name)
		}

		// Only set spec on creation — Job spec is immutable once the object exists.
		if job.CreationTimestamp.IsZero() {
			job.Spec = *app.Spec.Job
		}
		if job.Labels == nil {
			job.Labels = map[string]string{}
		}
		maps.Copy(job.Labels, LabelsForApplication(app.Name, version))

		return ctrlutil.SetControllerReference(app, job, scheme)
	})
	if err != nil {
		return err
	}
	if op != ctrlutil.OperationResultNone {
		log.FromContext(ctx).Info("Job reconciled", "operation", op, "name", job.Name)
	}
	return nil
}

// CleanupJob deletes the Job owned by the Application if it exists.
// This is called when transitioning away from Job mode to another WorkloadType.
func CleanupJob(ctx context.Context, c client.Client, app *workloadv1alpha1.Application) error {
	return CleanupOwnedResource(ctx, c, app, &batchv1.Job{}, "Job")
}

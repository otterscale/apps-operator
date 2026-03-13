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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
	"github.com/otterscale/workload-operator/internal/labels"
)

const (
	// ConditionTypeReady indicates whether all Application resources have been
	// successfully reconciled and the Deployment is available.
	ConditionTypeReady = "Ready"

	// ConditionTypeProgressing indicates the Deployment is rolling out new pods.
	ConditionTypeProgressing = "Progressing"

	// ReasonObservationError is used when a transient error prevents the controller
	// from reading the workload's current state.
	ReasonObservationError = "ObservationError"
)

// LabelsForApplication returns a standard set of labels for resources managed by this operator.
func LabelsForApplication(name, version string) map[string]string {
	return labels.Standard(name, "application", version)
}

// CleanupOwnedResource deletes a resource only if it exists and is owned by the given Application.
// It uses the OwnerReference UID to prevent accidentally deleting same-named resources
// created outside of this operator (e.g. by a workspace admin).
// This is called when transitioning between WorkloadTypes to remove stale resources.
func CleanupOwnedResource[T client.Object](ctx context.Context, c client.Client, app *workloadv1alpha1.Application, obj T, resourceType string) error {
	if err := c.Get(ctx, types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == app.UID {
			if err := c.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
			log.FromContext(ctx).Info(resourceType+" cleaned up", "name", obj.GetName())
			return nil
		}
	}
	log.FromContext(ctx).Info(resourceType+" not owned by this Application, skipping cleanup", "name", obj.GetName())
	return nil
}

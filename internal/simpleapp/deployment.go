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

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/otterscale/api/apps/v1alpha1"
)

// ReconcileDeployment ensures the Deployment exists and matches the desired state
// declared in SimpleApp.Spec.Deployment.
func ReconcileDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, app *appsv1alpha1.SimpleApp, version string) error {
	var deploySpec appsv1.DeploymentSpec
	if err := json.Unmarshal(app.Spec.Deployment.Raw, &deploySpec); err != nil {
		return &InvalidSpecError{Field: "deployment", Message: err.Error()}
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      app.Name,
			Namespace: app.Namespace,
		},
	}

	op, err := ctrlutil.CreateOrUpdate(ctx, c, deploy, func() error {
		deploy.Spec = deploySpec

		if deploy.Labels == nil {
			deploy.Labels = map[string]string{}
		}
		maps.Copy(deploy.Labels, LabelsForSimpleApp(app.Name, version))

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

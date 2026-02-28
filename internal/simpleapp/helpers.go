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
	"fmt"

	"github.com/otterscale/apps-operator/internal/labels"
)

const (
	// ConditionTypeReady indicates whether all SimpleApp resources have been
	// successfully reconciled and the Deployment is available.
	ConditionTypeReady = "Ready"

	// ConditionTypeProgressing indicates the Deployment is rolling out new pods.
	ConditionTypeProgressing = "Progressing"

	// ConditionTypeDegraded indicates the Deployment has not reached its
	// desired replica count or has failing pods.
	ConditionTypeDegraded = "Degraded"
)

// InvalidSpecError is a permanent error indicating that a field in SimpleAppSpec
// contains invalid JSON that cannot be deserialized into the expected Kubernetes type.
type InvalidSpecError struct {
	Field   string
	Message string
}

func (e *InvalidSpecError) Error() string {
	return fmt.Sprintf("invalid %s spec: %s", e.Field, e.Message)
}

// LabelsForSimpleApp returns a standard set of labels for resources managed by this operator.
func LabelsForSimpleApp(name, version string) map[string]string {
	return labels.Standard(name, "simpleapp", version)
}

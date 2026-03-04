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

package controller

import (
	"cmp"
	"context"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
	app "github.com/otterscale/workload-operator/internal/application"
)

// ApplicationReconciler reconciles an Application object.
// It ensures that the underlying Deployment, optional Service, and optional PersistentVolumeClaim
// match the desired state defined in the Application CR.
//
// The controller is intentionally kept thin: it orchestrates the reconciliation flow,
// while the actual resource synchronization logic resides in internal/application/.
type ApplicationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Version  string
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=workload.otterscale.io,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=workload.otterscale.io,resources=applications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is the main loop for the controller.
// It implements level-triggered reconciliation: Fetch -> Sync Resources -> Status Update.
//
// Deletion is handled entirely by Kubernetes garbage collection: all child resources
// are created with OwnerReferences pointing to the Application, so they are automatically
// cascade-deleted when the Application is removed. No finalizer is needed.
func (r *ApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.Name)
	ctx = log.IntoContext(ctx, logger)

	// 1. Fetch the Application instance
	var application workloadv1alpha1.Application
	if err := r.Get(ctx, req.NamespacedName, &application); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Reconcile all domain resources
	if err := r.reconcileResources(ctx, &application); err != nil {
		return r.handleReconcileError(ctx, &application, err)
	}

	// 3. Update Status
	if err := r.updateStatus(ctx, &application); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileResources orchestrates the domain-level resource sync in order.
func (r *ApplicationReconciler) reconcileResources(ctx context.Context, application *workloadv1alpha1.Application) error {
	if err := app.ReconcileDeployment(ctx, r.Client, r.Scheme, application, r.Version); err != nil {
		return err
	}
	if err := app.ReconcileService(ctx, r.Client, r.Scheme, application, r.Version); err != nil {
		return err
	}
	return app.ReconcilePVC(ctx, r.Client, r.Scheme, application, r.Version)
}

// handleReconcileError categorizes errors and updates status accordingly.
// Conflict errors are silently requeued; they are transient and expected during concurrent updates.
// Transient errors are returned to controller-runtime for exponential backoff retry.
func (r *ApplicationReconciler) handleReconcileError(ctx context.Context, application *workloadv1alpha1.Application, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("Conflict detected, requeuing", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}

	r.setReadyConditionFalse(ctx, application, "ReconcileError", err.Error())
	r.Recorder.Eventf(application, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", err.Error())
	return ctrl.Result{}, err
}

// setReadyConditionFalse updates the Ready condition to False via status patch.
// Errors are logged rather than propagated to avoid masking the original reconcile error.
func (r *ApplicationReconciler) setReadyConditionFalse(ctx context.Context, application *workloadv1alpha1.Application, reason, message string) {
	logger := log.FromContext(ctx)

	patch := client.MergeFrom(application.DeepCopy())
	meta.SetStatusCondition(&application.Status.Conditions, metav1.Condition{
		Type:               app.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: application.Generation,
	})
	application.Status.ObservedGeneration = application.Generation

	if err := r.Status().Patch(ctx, application, patch); err != nil {
		logger.Error(err, "Failed to patch Ready=False status condition", "reason", reason)
	}
}

// updateStatus calculates the status based on the current observed state and patches the resource.
func (r *ApplicationReconciler) updateStatus(ctx context.Context, application *workloadv1alpha1.Application) error {
	newStatus := application.Status.DeepCopy()
	newStatus.ObservedGeneration = application.Generation

	// Set resource references
	newStatus.DeploymentRef = &workloadv1alpha1.ResourceReference{
		Name:      application.Name,
		Namespace: application.Namespace,
	}

	if application.Spec.Service != nil {
		newStatus.ServiceRef = &workloadv1alpha1.ResourceReference{
			Name:      application.Name,
			Namespace: application.Namespace,
		}
	} else {
		newStatus.ServiceRef = nil
	}

	if application.Spec.PersistentVolumeClaim != nil {
		newStatus.PersistentVolumeClaimRef = &workloadv1alpha1.ResourceReference{
			Name:      application.Name,
			Namespace: application.Namespace,
		}
	} else {
		newStatus.PersistentVolumeClaimRef = nil
	}

	// Observe the Deployment status to derive the Application Ready condition
	readyStatus, readyReason, readyMessage := r.observeDeploymentStatus(ctx, application.Name, application.Namespace)
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               app.ConditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: application.Generation,
	})

	// Mirror Deployment Progressing condition
	progressingStatus, progressingReason, progressingMessage := r.observeDeploymentCondition(ctx, application.Name, application.Namespace, string(appsv1.DeploymentProgressing))
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               app.ConditionTypeProgressing,
		Status:             progressingStatus,
		Reason:             progressingReason,
		Message:            progressingMessage,
		ObservedGeneration: application.Generation,
	})

	// Sort conditions by type for stable ordering
	slices.SortFunc(newStatus.Conditions, func(a, b metav1.Condition) int {
		return cmp.Compare(a.Type, b.Type)
	})

	// Only patch if status has changed to reduce API server load
	if !equality.Semantic.DeepEqual(application.Status, *newStatus) {
		patch := client.MergeFrom(application.DeepCopy())
		application.Status = *newStatus
		if err := r.Status().Patch(ctx, application, patch); err != nil {
			return err
		}
		log.FromContext(ctx).Info("Application status updated")
		r.Recorder.Eventf(application, nil, corev1.EventTypeNormal, "Reconciled", "Reconcile",
			"Application resources reconciled")
	}

	return nil
}

// observeDeploymentStatus reads the Deployment's Available condition and maps it
// to the Application Ready condition.
func (r *ApplicationReconciler) observeDeploymentStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
		return metav1.ConditionFalse, "DeploymentNotFound", "waiting for Deployment to be created"
	}

	for _, c := range deploy.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			status := metav1.ConditionFalse
			if c.Status == corev1.ConditionTrue {
				status = metav1.ConditionTrue
			}
			return status, "Deployment" + c.Reason, c.Message
		}
	}

	return metav1.ConditionUnknown, "DeploymentPending", "Deployment has no Available condition yet"
}

// observeDeploymentCondition reads a specific condition from the Deployment.
func (r *ApplicationReconciler) observeDeploymentCondition(ctx context.Context, name, namespace, condType string) (metav1.ConditionStatus, string, string) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
		return metav1.ConditionUnknown, "DeploymentNotFound", "waiting for Deployment to be created"
	}

	for _, c := range deploy.Status.Conditions {
		if string(c.Type) == condType {
			status := metav1.ConditionUnknown
			switch c.Status {
			case corev1.ConditionTrue:
				status = metav1.ConditionTrue
			case corev1.ConditionFalse:
				status = metav1.ConditionFalse
			}
			return status, "Deployment" + c.Reason, c.Message
		}
	}

	return metav1.ConditionUnknown, "DeploymentPending", "Deployment has no " + condType + " condition yet"
}

// SetupWithManager registers the controller with the Manager and defines watches.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadv1alpha1.Application{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("application").
		Complete(r)
}

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
	"errors"
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

	appsv1alpha1 "github.com/otterscale/api/apps/v1alpha1"
	sa "github.com/otterscale/apps-operator/internal/simpleapp"
)

// SimpleAppReconciler reconciles a SimpleApp object.
// It ensures that the underlying Deployment, optional Service, and optional PersistentVolumeClaim
// match the desired state defined in the SimpleApp CR.
//
// The controller is intentionally kept thin: it orchestrates the reconciliation flow,
// while the actual resource synchronization logic resides in internal/simpleapp/.
type SimpleAppReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Version  string
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=apps.otterscale.io,resources=simpleapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.otterscale.io,resources=simpleapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.otterscale.io,resources=simpleapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is the main loop for the controller.
// It implements level-triggered reconciliation: Fetch -> Sync Resources -> Status Update.
//
// Deletion is handled entirely by Kubernetes garbage collection: all child resources
// are created with OwnerReferences pointing to the SimpleApp, so they are automatically
// cascade-deleted when the SimpleApp is removed. No finalizer is needed.
func (r *SimpleAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName(req.Name)
	ctx = log.IntoContext(ctx, logger)

	// 1. Fetch the SimpleApp instance
	var app appsv1alpha1.SimpleApp
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Reconcile all domain resources
	if err := r.reconcileResources(ctx, &app); err != nil {
		return r.handleReconcileError(ctx, &app, err)
	}

	// 3. Update Status
	if err := r.updateStatus(ctx, &app); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileResources orchestrates the domain-level resource sync in order.
func (r *SimpleAppReconciler) reconcileResources(ctx context.Context, app *appsv1alpha1.SimpleApp) error {
	if err := sa.ReconcileDeployment(ctx, r.Client, r.Scheme, app, r.Version); err != nil {
		return err
	}
	if err := sa.ReconcileService(ctx, r.Client, r.Scheme, app, r.Version); err != nil {
		return err
	}
	return sa.ReconcilePVC(ctx, r.Client, r.Scheme, app, r.Version)
}

// handleReconcileError categorizes errors and updates status accordingly.
// Permanent errors (e.g. invalid spec) do NOT requeue to avoid infinite loops.
// Conflict errors are silently requeued; they are transient and expected during concurrent updates.
// Transient errors are returned to controller-runtime for exponential backoff retry.
func (r *SimpleAppReconciler) handleReconcileError(ctx context.Context, app *appsv1alpha1.SimpleApp, err error) (ctrl.Result, error) {
	var ise *sa.InvalidSpecError
	if errors.As(err, &ise) {
		r.setReadyConditionFalse(ctx, app, "InvalidSpec", err.Error())
		r.Recorder.Eventf(app, nil, corev1.EventTypeWarning, "InvalidSpec", "Reconcile", err.Error())
		return ctrl.Result{}, nil
	}

	if apierrors.IsConflict(err) {
		log.FromContext(ctx).V(1).Info("Conflict detected, requeuing", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}

	r.setReadyConditionFalse(ctx, app, "ReconcileError", err.Error())
	r.Recorder.Eventf(app, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", err.Error())
	return ctrl.Result{}, err
}

// setReadyConditionFalse updates the Ready condition to False via status patch.
// Errors are logged rather than propagated to avoid masking the original reconcile error.
func (r *SimpleAppReconciler) setReadyConditionFalse(ctx context.Context, app *appsv1alpha1.SimpleApp, reason, message string) {
	logger := log.FromContext(ctx)

	patch := client.MergeFrom(app.DeepCopy())
	meta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type:               sa.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: app.Generation,
	})
	app.Status.ObservedGeneration = app.Generation

	if err := r.Status().Patch(ctx, app, patch); err != nil {
		logger.Error(err, "Failed to patch Ready=False status condition", "reason", reason)
	}
}

// updateStatus calculates the status based on the current observed state and patches the resource.
func (r *SimpleAppReconciler) updateStatus(ctx context.Context, app *appsv1alpha1.SimpleApp) error {
	newStatus := app.Status.DeepCopy()
	newStatus.ObservedGeneration = app.Generation

	// Set resource references
	newStatus.DeploymentRef = &appsv1alpha1.ResourceReference{
		Name:      app.Name,
		Namespace: app.Namespace,
	}

	if app.Spec.Service != nil {
		newStatus.ServiceRef = &appsv1alpha1.ResourceReference{
			Name:      app.Name,
			Namespace: app.Namespace,
		}
	} else {
		newStatus.ServiceRef = nil
	}

	if app.Spec.PersistentVolumeClaim != nil {
		newStatus.PersistentVolumeClaimRef = &appsv1alpha1.ResourceReference{
			Name:      app.Name,
			Namespace: app.Namespace,
		}
	} else {
		newStatus.PersistentVolumeClaimRef = nil
	}

	// Observe the Deployment status to derive the SimpleApp Ready condition
	readyStatus, readyReason, readyMessage := r.observeDeploymentStatus(ctx, app.Name, app.Namespace)
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               sa.ConditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: app.Generation,
	})

	// Mirror Deployment Progressing condition
	progressingStatus, progressingReason, progressingMessage := r.observeDeploymentCondition(ctx, app.Name, app.Namespace, string(appsv1.DeploymentProgressing))
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               sa.ConditionTypeProgressing,
		Status:             progressingStatus,
		Reason:             progressingReason,
		Message:            progressingMessage,
		ObservedGeneration: app.Generation,
	})

	// Sort conditions by type for stable ordering
	slices.SortFunc(newStatus.Conditions, func(a, b metav1.Condition) int {
		return cmp.Compare(a.Type, b.Type)
	})

	// Only patch if status has changed to reduce API server load
	if !equality.Semantic.DeepEqual(app.Status, *newStatus) {
		patch := client.MergeFrom(app.DeepCopy())
		app.Status = *newStatus
		if err := r.Status().Patch(ctx, app, patch); err != nil {
			return err
		}
		log.FromContext(ctx).Info("SimpleApp status updated")
		r.Recorder.Eventf(app, nil, corev1.EventTypeNormal, "Reconciled", "Reconcile",
			"SimpleApp resources reconciled")
	}

	return nil
}

// observeDeploymentStatus reads the Deployment's Available condition and maps it
// to the SimpleApp Ready condition.
func (r *SimpleAppReconciler) observeDeploymentStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
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
func (r *SimpleAppReconciler) observeDeploymentCondition(ctx context.Context, name, namespace, condType string) (metav1.ConditionStatus, string, string) {
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
func (r *SimpleAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.SimpleApp{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("simpleapp").
		Complete(r)
}

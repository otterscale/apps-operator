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
	batchv1 "k8s.io/api/batch/v1"
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
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
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
// It dispatches to the appropriate workload branch based on WorkloadType, and
// cleans up stale resources left over from a previous mode.
func (r *ApplicationReconciler) reconcileResources(ctx context.Context, application *workloadv1alpha1.Application) error {
	switch application.Spec.WorkloadType {
	case workloadv1alpha1.WorkloadTypeCronJob:
		// Transitioning from Deployment/Job: remove any pre-existing resources.
		if err := app.CleanupDeployment(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupService(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupPVC(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupJob(ctx, r.Client, application); err != nil {
			return err
		}
		return app.ReconcileCronJob(ctx, r.Client, r.Scheme, application, r.Version)
	case workloadv1alpha1.WorkloadTypeJob:
		// Transitioning from Deployment/CronJob: remove any pre-existing resources.
		if err := app.CleanupDeployment(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupService(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupPVC(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupCronJob(ctx, r.Client, application); err != nil {
			return err
		}
		return app.ReconcileJob(ctx, r.Client, r.Scheme, application, r.Version)
	default: // WorkloadTypeDeployment
		// Transitioning from CronJob/Job: remove any pre-existing workload.
		if err := app.CleanupCronJob(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.CleanupJob(ctx, r.Client, application); err != nil {
			return err
		}
		if err := app.ReconcileDeployment(ctx, r.Client, r.Scheme, application, r.Version); err != nil {
			return err
		}
		if err := app.ReconcileService(ctx, r.Client, r.Scheme, application, r.Version); err != nil {
			return err
		}
		return app.ReconcilePVC(ctx, r.Client, r.Scheme, application, r.Version)
	}
}

// handleReconcileError categorizes errors and updates status accordingly.
// Conflict errors are silently requeued; they are transient and expected during concurrent updates.
// Transient errors are returned to controller-runtime for exponential backoff retry.
func (r *ApplicationReconciler) handleReconcileError(ctx context.Context, application *workloadv1alpha1.Application, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		log.FromContext(ctx).Info("Conflict detected, requeuing", "error", err)
		return ctrl.Result{Requeue: true}, nil
	}

	_ = r.setReadyConditionFalse(ctx, application, "ReconcileError", err.Error())
	r.Recorder.Eventf(application, nil, corev1.EventTypeWarning, "ReconcileError", "Reconcile", err.Error())
	return ctrl.Result{}, err
}

// setReadyConditionFalse updates the Ready condition to False via status patch.
// Returns the patch error so callers can decide whether to retry.
func (r *ApplicationReconciler) setReadyConditionFalse(ctx context.Context, application *workloadv1alpha1.Application, reason, message string) error {
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
		log.FromContext(ctx).Error(err, "Failed to patch Ready=False status condition", "reason", reason)
		return err
	}
	return nil
}

// updateStatus calculates the status based on the current observed state and patches the resource.
func (r *ApplicationReconciler) updateStatus(ctx context.Context, application *workloadv1alpha1.Application) error {
	newStatus := application.Status.DeepCopy()
	newStatus.ObservedGeneration = application.Generation

	// Set workload-type-scoped resource references and derive Ready/Progressing conditions.
	var readyStatus metav1.ConditionStatus
	var readyReason, readyMessage string
	// Initialize Progressing fields to safe defaults. They are only written to status
	// for Deployment workloads (guarded below); explicit initialization avoids relying
	// on an implicit zero-value guard against future refactoring.
	progressingStatus := metav1.ConditionUnknown
	progressingReason := "WorkloadTypePending"
	progressingMessage := "Progressing condition is not applicable for this workload type"

	switch application.Spec.WorkloadType {
	case workloadv1alpha1.WorkloadTypeCronJob:
		// Remove Progressing condition — it is not applicable for CronJob workloads and
		// must be explicitly cleared when transitioning from Deployment mode.
		meta.RemoveStatusCondition(&newStatus.Conditions, app.ConditionTypeProgressing)
		newStatus.DeploymentRef = nil
		newStatus.ServiceRef = nil
		newStatus.PersistentVolumeClaimRef = nil
		newStatus.JobRef = nil
		newStatus.CronJobRef = &workloadv1alpha1.ResourceReference{
			Name:      application.Name,
			Namespace: application.Namespace,
		}
		readyStatus, readyReason, readyMessage = r.observeCronJobStatus(ctx, application.Name, application.Namespace)
	case workloadv1alpha1.WorkloadTypeJob:
		// Remove Progressing condition — not applicable for one-off Job workloads.
		meta.RemoveStatusCondition(&newStatus.Conditions, app.ConditionTypeProgressing)
		newStatus.DeploymentRef = nil
		newStatus.ServiceRef = nil
		newStatus.PersistentVolumeClaimRef = nil
		newStatus.CronJobRef = nil
		newStatus.JobRef = &workloadv1alpha1.ResourceReference{
			Name:      application.Name,
			Namespace: application.Namespace,
		}
		readyStatus, readyReason, readyMessage = r.observeJobStatus(ctx, application.Name, application.Namespace)
	default: // WorkloadTypeDeployment
		newStatus.CronJobRef = nil
		newStatus.JobRef = nil
		newStatus.DeploymentRef = &workloadv1alpha1.ResourceReference{
			Name:      application.Name,
			Namespace: application.Namespace,
		}
		if application.Spec.DeploymentConfig != nil && application.Spec.DeploymentConfig.Service != nil {
			newStatus.ServiceRef = &workloadv1alpha1.ResourceReference{
				Name:      application.Name,
				Namespace: application.Namespace,
			}
		} else {
			newStatus.ServiceRef = nil
		}
		if application.Spec.DeploymentConfig != nil && application.Spec.DeploymentConfig.PersistentVolumeClaim != nil {
			newStatus.PersistentVolumeClaimRef = &workloadv1alpha1.ResourceReference{
				Name:      application.Name,
				Namespace: application.Namespace,
			}
		} else {
			newStatus.PersistentVolumeClaimRef = nil
		}
		readyStatus, readyReason, readyMessage, progressingStatus, progressingReason, progressingMessage = r.observeDeploymentStatuses(ctx, application.Name, application.Namespace)
	}

	// Observe the workload status to derive the Application Ready condition
	meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
		Type:               app.ConditionTypeReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: application.Generation,
	})

	// Mirror Deployment Progressing condition (only applicable for Deployment workloads)
	if application.Spec.WorkloadType == workloadv1alpha1.WorkloadTypeDeployment {
		meta.SetStatusCondition(&newStatus.Conditions, metav1.Condition{
			Type:               app.ConditionTypeProgressing,
			Status:             progressingStatus,
			Reason:             progressingReason,
			Message:            progressingMessage,
			ObservedGeneration: application.Generation,
		})
	}

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

// observeDeploymentStatuses fetches the Deployment once and derives both the Ready
// and Progressing condition tuples, avoiding redundant API server calls.
// Transient errors (non-NotFound) are surfaced as ConditionUnknown / "ObservationError"
// rather than being silently misreported as "DeploymentNotFound".
func (r *ApplicationReconciler) observeDeploymentStatuses(ctx context.Context, name, namespace string) (
	readyStatus metav1.ConditionStatus, readyReason, readyMsg string,
	progressingStatus metav1.ConditionStatus, progressingReason, progressingMsg string,
) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "DeploymentNotFound", "waiting for Deployment to be created",
				metav1.ConditionUnknown, "DeploymentNotFound", "waiting for Deployment to be created"
		}
		msg := err.Error()
		return metav1.ConditionUnknown, "ObservationError", msg,
			metav1.ConditionUnknown, "ObservationError", msg
	}

	readyStatus = metav1.ConditionUnknown
	readyReason = "DeploymentPending"
	readyMsg = "Deployment has no Available condition yet"
	progressingStatus = metav1.ConditionUnknown
	progressingReason = "DeploymentPending"
	progressingMsg = "Deployment has no " + string(appsv1.DeploymentProgressing) + " condition yet"

	for _, c := range deploy.Status.Conditions {
		switch c.Type {
		case appsv1.DeploymentAvailable:
			if c.Status == corev1.ConditionTrue {
				readyStatus = metav1.ConditionTrue
			} else {
				readyStatus = metav1.ConditionFalse
			}
			readyReason = "Deployment" + c.Reason
			readyMsg = c.Message
		case appsv1.DeploymentProgressing:
			switch c.Status {
			case corev1.ConditionTrue:
				progressingStatus = metav1.ConditionTrue
			case corev1.ConditionFalse:
				progressingStatus = metav1.ConditionFalse
			}
			progressingReason = "Deployment" + c.Reason
			progressingMsg = c.Message
		}
	}

	return
}

// observeCronJobStatus reads the CronJob and derives the Application Ready condition.
// A CronJob is considered Ready once it has been scheduled at least once.
// Transient errors (non-NotFound) are surfaced as ConditionUnknown / "ObservationError".
func (r *ApplicationReconciler) observeCronJobStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
	var cj batchv1.CronJob
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &cj); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "CronJobNotFound", "waiting for CronJob to be created"
		}
		return metav1.ConditionUnknown, "ObservationError", err.Error()
	}
	if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
		return metav1.ConditionFalse, "CronJobSuspended", "CronJob is suspended and will not be scheduled"
	}
	if cj.Status.LastScheduleTime != nil {
		return metav1.ConditionTrue, "CronJobScheduled", "CronJob has been scheduled at least once"
	}
	return metav1.ConditionUnknown, "CronJobPending", "CronJob has not been scheduled yet"
}

// observeJobStatus reads the Job and derives the Application Ready condition.
// A Job is considered Ready (Complete) when it has the Complete condition set to True.
// A failed Job surfaces as Ready=False with reason JobFailed.
// Transient errors (non-NotFound) are surfaced as ConditionUnknown / "ObservationError".
func (r *ApplicationReconciler) observeJobStatus(ctx context.Context, name, namespace string) (metav1.ConditionStatus, string, string) {
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse, "JobNotFound", "waiting for Job to be created"
		}
		return metav1.ConditionUnknown, "ObservationError", err.Error()
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return metav1.ConditionTrue, "JobComplete", "Job completed successfully"
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return metav1.ConditionFalse, "JobFailed", c.Message
		}
	}
	return metav1.ConditionUnknown, "JobRunning", "Job is in progress"
}

// SetupWithManager registers the controller with the Manager and defines watches.
func (r *ApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workloadv1alpha1.Application{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.CronJob{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("application").
		Complete(r)
}

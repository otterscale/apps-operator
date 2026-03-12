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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workloadv1alpha1 "github.com/otterscale/api/workload/v1alpha1"
	"github.com/otterscale/workload-operator/internal/labels"
)

var _ = Describe("Application Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	var (
		ctx          context.Context
		reconciler   *ApplicationReconciler
		application  *workloadv1alpha1.Application
		resourceName string
		namespace    *corev1.Namespace
	)

	// --- Helpers ---

	one := int32(1)

	defaultDeploymentSpec := func() appsv1.DeploymentSpec {
		return appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "test"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "nginx:latest",
					}},
				},
			},
		}
	}

	makeApplication := func(name, ns string, mods ...func(*workloadv1alpha1.Application)) *workloadv1alpha1.Application {
		deploySpec := defaultDeploymentSpec()
		a := &workloadv1alpha1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: workloadv1alpha1.ApplicationSpec{
				DeploymentConfig: &workloadv1alpha1.DeploymentConfig{
					Deployment: deploySpec,
				},
			},
		}
		for _, mod := range mods {
			mod(a)
		}
		return a
	}

	executeReconcile := func() {
		nsName := types.NamespacedName{Name: resourceName, Namespace: namespace.Name}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nsName})
		Expect(err).NotTo(HaveOccurred())
	}

	fetchResource := func(obj client.Object, name, ns string) {
		key := types.NamespacedName{Name: name, Namespace: ns}
		Eventually(func() error {
			return k8sClient.Get(ctx, key, obj)
		}, timeout, interval).Should(Succeed())
	}

	// --- Lifecycle ---

	BeforeEach(func() {
		ctx = context.Background()
		resourceName = "app-" + string(uuid.NewUUID())[:8]

		namespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-" + string(uuid.NewUUID())[:8]},
		}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

		reconciler = &ApplicationReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Version:  "test",
			Recorder: events.NewFakeRecorder(100),
		}
		application = makeApplication(resourceName, namespace.Name)
	})

	JustBeforeEach(func() {
		Expect(k8sClient.Create(ctx, application)).To(Succeed())
	})

	AfterEach(func() {
		key := types.NamespacedName{Name: resourceName, Namespace: namespace.Name}
		if err := k8sClient.Get(ctx, key, application); err == nil {
			Expect(k8sClient.Delete(ctx, application)).To(Succeed())
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, key, application))
			}, timeout, interval).Should(BeTrue())
		}
	})

	// --- Tests ---

	Context("Basic Reconciliation", func() {
		It("should create a Deployment and update status", func() {
			executeReconcile()

			By("Verifying the Deployment is created")
			var deploy appsv1.Deployment
			fetchResource(&deploy, resourceName, namespace.Name)
			Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "workload-operator"))
			Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "application"))
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

			By("Verifying OwnerReference is set")
			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Name).To(Equal(resourceName))

			By("Verifying status updates")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.ObservedGeneration).To(Equal(application.Generation))
			Expect(application.Status.DeploymentRef).NotTo(BeNil())
			Expect(application.Status.DeploymentRef.Name).To(Equal(resourceName))
			Expect(application.Status.DeploymentRef.Namespace).To(Equal(namespace.Name))
			Expect(application.Status.ServiceRef).To(BeNil())
			Expect(application.Status.PersistentVolumeClaimRef).To(BeNil())
		})
	})

	Context("Optional Service Lifecycle", func() {
		BeforeEach(func() {
			application = makeApplication(resourceName, namespace.Name, func(a *workloadv1alpha1.Application) {
				a.Spec.DeploymentConfig.Service = &corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port:       80,
						TargetPort: intstr.FromInt32(8080),
					}},
					Selector: map[string]string{"app": "test"},
				}
			})
		})

		It("should manage Service creation and deletion", func() {
			executeReconcile()

			By("Verifying Service creation")
			var svc corev1.Service
			fetchResource(&svc, resourceName, namespace.Name)
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
			Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "workload-operator"))

			By("Verifying status has ServiceRef")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.ServiceRef).NotTo(BeNil())
			Expect(application.Status.ServiceRef.Name).To(Equal(resourceName))

			By("Removing Service from spec")
			fetchResource(application, resourceName, namespace.Name)
			application.Spec.DeploymentConfig.Service = nil
			Expect(k8sClient.Update(ctx, application)).To(Succeed())

			executeReconcile()

			By("Verifying Service is deleted")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
				Name: resourceName, Namespace: namespace.Name,
			}, &svc))).To(BeTrue())

			By("Verifying ServiceRef is cleared in status")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.ServiceRef).To(BeNil())
		})
	})

	Context("Optional PVC Lifecycle", func() {
		BeforeEach(func() {
			application = makeApplication(resourceName, namespace.Name, func(a *workloadv1alpha1.Application) {
				a.Spec.DeploymentConfig.MountPath = "/data"
				a.Spec.DeploymentConfig.PersistentVolumeClaim = &corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				}
			})
		})

		It("should manage PVC creation and deletion", func() {
			executeReconcile()

			By("Verifying PVC creation")
			var pvc corev1.PersistentVolumeClaim
			fetchResource(&pvc, resourceName, namespace.Name)
			Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
			Expect(pvc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "workload-operator"))

			By("Verifying status has PVCRef")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.PersistentVolumeClaimRef).NotTo(BeNil())
			Expect(application.Status.PersistentVolumeClaimRef.Name).To(Equal(resourceName))

			By("Removing PVC from spec")
			fetchResource(application, resourceName, namespace.Name)
			application.Spec.DeploymentConfig.PersistentVolumeClaim = nil
			application.Spec.DeploymentConfig.MountPath = ""
			Expect(k8sClient.Update(ctx, application)).To(Succeed())

			executeReconcile()

			By("Verifying PVC has a deletion timestamp")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: resourceName, Namespace: namespace.Name,
				}, &pvc)
				if errors.IsNotFound(err) {
					return true
				}
				return err == nil && !pvc.DeletionTimestamp.IsZero()
			}, timeout, interval).Should(BeTrue())

			By("Verifying PVCRef is cleared in status")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.PersistentVolumeClaimRef).To(BeNil())
		})
	})

	Context("CronJob Workload", func() {
		BeforeEach(func() {
			application = &workloadv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace.Name},
				Spec: workloadv1alpha1.ApplicationSpec{
					WorkloadType: workloadv1alpha1.WorkloadTypeCronJob,
					CronJob: &batchv1.CronJobSpec{
						Schedule:                   "0 2 * * *",
						SuccessfulJobsHistoryLimit: func() *int32 { v := int32(3); return &v }(),
						FailedJobsHistoryLimit:     func() *int32 { v := int32(1); return &v }(),
						JobTemplate: batchv1.JobTemplateSpec{
							Spec: batchv1.JobSpec{
								Template: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										RestartPolicy: corev1.RestartPolicyOnFailure,
										Containers: []corev1.Container{{
											Name:    "worker",
											Image:   "busybox:1.37",
											Command: []string{"/bin/sh", "-c", "echo done"},
										}},
									},
								},
							},
						},
					},
				},
			}
		})

		It("should create a CronJob with correct labels and OwnerReference", func() {
			executeReconcile()

			By("Verifying the CronJob is created")
			var cj batchv1.CronJob
			fetchResource(&cj, resourceName, namespace.Name)
			Expect(cj.Spec.Schedule).To(Equal("0 2 * * *"))
			Expect(cj.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "workload-operator"))
			Expect(cj.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "application"))

			By("Verifying OwnerReference is set")
			Expect(cj.OwnerReferences).To(HaveLen(1))
			Expect(cj.OwnerReferences[0].Name).To(Equal(resourceName))

			By("Verifying status: CronJobRef set, all others nil")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.CronJobRef).NotTo(BeNil())
			Expect(application.Status.CronJobRef.Name).To(Equal(resourceName))
			Expect(application.Status.DeploymentRef).To(BeNil())
			Expect(application.Status.ServiceRef).To(BeNil())
			Expect(application.Status.PersistentVolumeClaimRef).To(BeNil())
			Expect(application.Status.JobRef).To(BeNil())
		})

		It("should not set a Progressing condition for CronJob workloads", func() {
			executeReconcile()

			fetchResource(application, resourceName, namespace.Name)
			for _, c := range application.Status.Conditions {
				Expect(c.Type).NotTo(Equal("Progressing"),
					"Progressing condition must not be present for CronJob workloads")
			}
		})

		It("should return an error when spec.cronJob is nil", func() {
			// Bypass API validation by directly patching the stored object after creation
			// so we can verify controller-side nil guard.
			Expect(k8sClient.Create(ctx, &workloadv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName + "-nil",
					Namespace: namespace.Name,
				},
				Spec: workloadv1alpha1.ApplicationSpec{
					WorkloadType: workloadv1alpha1.WorkloadTypeCronJob,
					CronJob: &batchv1.CronJobSpec{
						Schedule: "* * * * *",
						JobTemplate: batchv1.JobTemplateSpec{
							Spec: batchv1.JobSpec{
								Template: corev1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										RestartPolicy: corev1.RestartPolicyOnFailure,
										Containers:    []corev1.Container{{Name: "c", Image: "busybox"}},
									},
								},
							},
						},
					},
				},
			})).To(Succeed())

			// Construct a reconciler that holds a fake Application with nil CronJob
			// to exercise the nil-guard path in ReconcileCronJob directly.
			nilApp := &workloadv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-nil", Namespace: namespace.Name},
				Spec: workloadv1alpha1.ApplicationSpec{
					WorkloadType: workloadv1alpha1.WorkloadTypeCronJob,
					CronJob:      nil, // intentionally nil
				},
			}
			// Patch the live object's spec to have nil cronJob field so Reconcile
			// fetches it from the API and hits the guard.
			// Instead, call reconcileResources directly to test the guard path.
			err := reconciler.reconcileResources(ctx, nilApp)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.cronJob is nil"))
		})
	})

	Context("Job Workload", func() {
		BeforeEach(func() {
			application = &workloadv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace.Name},
				Spec: workloadv1alpha1.ApplicationSpec{
					WorkloadType: workloadv1alpha1.WorkloadTypeJob,
					Job: &batchv1.JobSpec{
						BackoffLimit:          func() *int32 { v := int32(2); return &v }(),
						ActiveDeadlineSeconds: func() *int64 { v := int64(300); return &v }(),
						Template: corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								RestartPolicy: corev1.RestartPolicyOnFailure,
								Containers: []corev1.Container{{
									Name:    "task",
									Image:   "busybox:1.37",
									Command: []string{"/bin/sh", "-c", "echo done"},
								}},
							},
						},
					},
				},
			}
		})

		It("should create a Job with correct labels and OwnerReference", func() {
			executeReconcile()

			By("Verifying the Job is created")
			var job batchv1.Job
			fetchResource(&job, resourceName, namespace.Name)
			Expect(job.Spec.BackoffLimit).NotTo(BeNil())
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(2)))
			Expect(job.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "workload-operator"))
			Expect(job.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "application"))

			By("Verifying OwnerReference is set")
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Name).To(Equal(resourceName))

			By("Verifying status: JobRef set, all others nil")
			fetchResource(application, resourceName, namespace.Name)
			Expect(application.Status.JobRef).NotTo(BeNil())
			Expect(application.Status.JobRef.Name).To(Equal(resourceName))
			Expect(application.Status.DeploymentRef).To(BeNil())
			Expect(application.Status.ServiceRef).To(BeNil())
			Expect(application.Status.PersistentVolumeClaimRef).To(BeNil())
			Expect(application.Status.CronJobRef).To(BeNil())
		})

		It("should not set a Progressing condition for Job workloads", func() {
			executeReconcile()

			fetchResource(application, resourceName, namespace.Name)
			for _, c := range application.Status.Conditions {
				Expect(c.Type).NotTo(Equal("Progressing"),
					"Progressing condition must not be present for Job workloads")
			}
		})

		It("should return an error when spec.job is nil", func() {
			nilApp := &workloadv1alpha1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace.Name},
				Spec: workloadv1alpha1.ApplicationSpec{
					WorkloadType: workloadv1alpha1.WorkloadTypeJob,
					Job:          nil, // intentionally nil
				},
			}
			err := reconciler.reconcileResources(ctx, nilApp)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.job is nil"))
		})

		It("should not overwrite Job spec on subsequent reconciles", func() {
			executeReconcile()

			By("Modifying the in-memory spec and reconciling again")
			fetchResource(application, resourceName, namespace.Name)
			newBackoff := int32(99)
			application.Spec.Job.BackoffLimit = &newBackoff
			// Do NOT update the API object — simulate a reconcile loop where
			// the controller sees the original API state.
			executeReconcile()

			By("Verifying the Job spec was not overwritten")
			var job batchv1.Job
			fetchResource(&job, resourceName, namespace.Name)
			// BackoffLimit should still be 2 (from creation), not 99
			Expect(*job.Spec.BackoffLimit).To(Equal(int32(2)))
		})
	})

	Context("Error Handling", func() {
		It("should requeue on transient errors", func() {
			executeReconcile()

			By("Simulating a transient error through handleReconcileError")
			fetchResource(application, resourceName, namespace.Name)
			_, err := reconciler.handleReconcileError(ctx, application,
				fmt.Errorf("connection refused"))

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("connection refused"))
		})
	})

	Context("Domain Helpers", func() {
		It("should generate correct labels", func() {
			appLabels := labels.Standard("my-app", "application", "v1")
			Expect(appLabels).To(HaveKeyWithValue(labels.Name, "my-app"))
			Expect(appLabels).To(HaveKeyWithValue(labels.Version, "v1"))
			Expect(appLabels).To(HaveKeyWithValue(labels.Component, "application"))
			Expect(appLabels).To(HaveKeyWithValue(labels.PartOf, "otterscale-system"))
			Expect(appLabels).To(HaveKeyWithValue(labels.ManagedBy, "workload-operator"))
		})
	})
})

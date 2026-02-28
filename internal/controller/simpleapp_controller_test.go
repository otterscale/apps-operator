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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/otterscale/api/apps/v1alpha1"
	"github.com/otterscale/apps-operator/internal/labels"
	sa "github.com/otterscale/apps-operator/internal/simpleapp"
)

var _ = Describe("SimpleApp Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	var (
		ctx          context.Context
		reconciler   *SimpleAppReconciler
		app          *appsv1alpha1.SimpleApp
		resourceName string
		namespace    *corev1.Namespace
	)

	// --- Helpers ---

	toRawExtension := func(obj any) *runtime.RawExtension {
		raw, err := json.Marshal(obj)
		Expect(err).NotTo(HaveOccurred())
		return &runtime.RawExtension{Raw: raw}
	}

	defaultDeploymentSpec := func() appsv1.DeploymentSpec {
		return appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
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

	makeSimpleApp := func(name, ns string, mods ...func(*appsv1alpha1.SimpleApp)) *appsv1alpha1.SimpleApp {
		a := &appsv1alpha1.SimpleApp{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1alpha1.SimpleAppSpec{
				Deployment: *toRawExtension(defaultDeploymentSpec()),
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
		resourceName = "sa-" + string(uuid.NewUUID())[:8]

		namespace = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "ns-" + string(uuid.NewUUID())[:8]},
		}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

		reconciler = &SimpleAppReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Version:  "test",
			Recorder: events.NewFakeRecorder(100),
		}
		app = makeSimpleApp(resourceName, namespace.Name)
	})

	JustBeforeEach(func() {
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
	})

	AfterEach(func() {
		key := types.NamespacedName{Name: resourceName, Namespace: namespace.Name}
		if err := k8sClient.Get(ctx, key, app); err == nil {
			Expect(k8sClient.Delete(ctx, app)).To(Succeed())
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, key, app))
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
			Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "apps-operator"))
			Expect(deploy.Labels).To(HaveKeyWithValue("app.kubernetes.io/component", "simpleapp"))
			Expect(*deploy.Spec.Replicas).To(Equal(int32(1)))

			By("Verifying OwnerReference is set")
			Expect(deploy.OwnerReferences).To(HaveLen(1))
			Expect(deploy.OwnerReferences[0].Name).To(Equal(resourceName))

			By("Verifying status updates")
			fetchResource(app, resourceName, namespace.Name)
			Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
			Expect(app.Status.DeploymentRef).NotTo(BeNil())
			Expect(app.Status.DeploymentRef.Name).To(Equal(resourceName))
			Expect(app.Status.DeploymentRef.Namespace).To(Equal(namespace.Name))
			Expect(app.Status.ServiceRef).To(BeNil())
			Expect(app.Status.PersistentVolumeClaimRef).To(BeNil())
		})
	})

	Context("Optional Service Lifecycle", func() {
		BeforeEach(func() {
			app = makeSimpleApp(resourceName, namespace.Name, func(a *appsv1alpha1.SimpleApp) {
				a.Spec.Service = toRawExtension(corev1.ServiceSpec{
					Ports: []corev1.ServicePort{{
						Port:       80,
						TargetPort: intstr.FromInt32(8080),
					}},
					Selector: map[string]string{"app": "test"},
				})
			})
		})

		It("should manage Service creation and deletion", func() {
			executeReconcile()

			By("Verifying Service creation")
			var svc corev1.Service
			fetchResource(&svc, resourceName, namespace.Name)
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
			Expect(svc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "apps-operator"))

			By("Verifying status has ServiceRef")
			fetchResource(app, resourceName, namespace.Name)
			Expect(app.Status.ServiceRef).NotTo(BeNil())
			Expect(app.Status.ServiceRef.Name).To(Equal(resourceName))

			By("Removing Service from spec")
			fetchResource(app, resourceName, namespace.Name)
			app.Spec.Service = nil
			Expect(k8sClient.Update(ctx, app)).To(Succeed())

			executeReconcile()

			By("Verifying Service is deleted")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
				Name: resourceName, Namespace: namespace.Name,
			}, &svc))).To(BeTrue())

			By("Verifying ServiceRef is cleared in status")
			fetchResource(app, resourceName, namespace.Name)
			Expect(app.Status.ServiceRef).To(BeNil())
		})
	})

	Context("Optional PVC Lifecycle", func() {
		BeforeEach(func() {
			app = makeSimpleApp(resourceName, namespace.Name, func(a *appsv1alpha1.SimpleApp) {
				a.Spec.PersistentVolumeClaim = toRawExtension(corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				})
			})
		})

		It("should manage PVC creation and deletion", func() {
			executeReconcile()

			By("Verifying PVC creation")
			var pvc corev1.PersistentVolumeClaim
			fetchResource(&pvc, resourceName, namespace.Name)
			Expect(pvc.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
			Expect(pvc.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "apps-operator"))

			By("Verifying status has PVCRef")
			fetchResource(app, resourceName, namespace.Name)
			Expect(app.Status.PersistentVolumeClaimRef).NotTo(BeNil())
			Expect(app.Status.PersistentVolumeClaimRef.Name).To(Equal(resourceName))

			By("Removing PVC from spec")
			fetchResource(app, resourceName, namespace.Name)
			app.Spec.PersistentVolumeClaim = nil
			Expect(k8sClient.Update(ctx, app)).To(Succeed())

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
			fetchResource(app, resourceName, namespace.Name)
			Expect(app.Status.PersistentVolumeClaimRef).To(BeNil())
		})
	})

	Context("Invalid Spec Handling", func() {
		It("should report InvalidSpecError for malformed deployment spec", func() {
			By("Verifying that InvalidSpecError is classified as permanent (no requeue)")
			var ise *sa.InvalidSpecError
			Expect(stderrors.As(&sa.InvalidSpecError{Field: "deployment", Message: "bad"}, &ise)).To(BeTrue())
			Expect(ise.Error()).To(ContainSubstring("invalid deployment spec"))
		})

		It("should set Ready=False via handleReconcileError on InvalidSpecError", func() {
			executeReconcile()

			By("Simulating an InvalidSpecError through handleReconcileError")
			fetchResource(app, resourceName, namespace.Name)
			result, err := reconciler.handleReconcileError(ctx, app,
				&sa.InvalidSpecError{Field: "deployment", Message: "cannot unmarshal"})

			Expect(err).NotTo(HaveOccurred()) // permanent error: no requeue
			Expect(result.Requeue).To(BeFalse())

			By("Verifying the status condition")
			fetchResource(app, resourceName, namespace.Name)
			readyCond := meta.FindStatusCondition(app.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("InvalidSpec"))
			Expect(readyCond.Message).To(ContainSubstring("cannot unmarshal"))
		})

		It("should requeue on transient errors", func() {
			executeReconcile()

			By("Simulating a transient error through handleReconcileError")
			fetchResource(app, resourceName, namespace.Name)
			_, err := reconciler.handleReconcileError(ctx, app,
				fmt.Errorf("connection refused"))

			Expect(err).To(HaveOccurred()) // transient error: requeue
			Expect(err.Error()).To(ContainSubstring("connection refused"))
		})
	})

	Context("Domain Helpers", func() {
		It("should generate correct labels", func() {
			appLabels := sa.LabelsForSimpleApp("my-app", "v1")
			Expect(appLabels).To(HaveKeyWithValue(labels.Name, "my-app"))
			Expect(appLabels).To(HaveKeyWithValue(labels.Version, "v1"))
			Expect(appLabels).To(HaveKeyWithValue(labels.Component, "simpleapp"))
			Expect(appLabels).To(HaveKeyWithValue(labels.PartOf, "otterscale-system"))
			Expect(appLabels).To(HaveKeyWithValue(labels.ManagedBy, "apps-operator"))
		})
	})
})

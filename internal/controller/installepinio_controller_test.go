/*
Copyright 2026.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	epiniov1alpha1 "github.com/epinio/install-operator/api/v1alpha1"
)

var _ = Describe("InstallEpinio Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "system",
		}
		installepinio := &epiniov1alpha1.InstallEpinio{}
		var reconciler *InstallEpinioReconciler

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}})).To(SatisfyAny(
				Succeed(),
				WithTransform(errors.IsAlreadyExists, BeTrue()),
			))
			reconciler = &InstallEpinioReconciler{
				Client:                  k8sClient,
				Scheme:                  k8sClient.Scheme(),
				ControllerNamespace:     "system",
				InstallerServiceAccount: "installer-sa",
				InstallerHelmImage:      "example.com/helm:test",
			}

			By("creating the custom resource for the Kind InstallEpinio")
			err := k8sClient.Get(ctx, typeNamespacedName, installepinio)
			if err != nil && errors.IsNotFound(err) {
				resource := &epiniov1alpha1.InstallEpinio{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "system",
					},
					Spec: epiniov1alpha1.InstallEpinioSpec{
						Domain:          "demo.example.test",
						TargetNamespace: "epinio",
						Version:         "1.0.0",
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &epiniov1alpha1.InstallEpinio{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if errors.IsNotFound(err) {
				return
			}
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance InstallEpinio")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			if err != nil && !errors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, typeNamespacedName, &epiniov1alpha1.InstallEpinio{}))
			}).Should(BeTrue())
		})

		It("creates owned install resources and sets a progressing condition", func() {
			By("Reconciling once to add the cleanup finalizer")
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Reconciling again to create the install resources")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-installer-system-test-resource", Namespace: "system"}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "EPINIO_VERSION", Value: "1.0.0"}))
			Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("installer-sa"))
			Expect(job.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/helm:test"))
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Name).To(Equal(resourceName))

			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-install-script-system-test-resource", Namespace: "system"}, configMap)).To(Succeed())
			Expect(configMap.OwnerReferences).To(HaveLen(1))
			Expect(configMap.OwnerReferences[0].Name).To(Equal(resourceName))

			updated := &epiniov1alpha1.InstallEpinio{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, "Progressing")).To(BeTrue())
			Expect(updated.Finalizers).To(ContainElement(installCleanupFinalizer))
		})

		It("marks the install available when the Job succeeds", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-installer-system-test-resource", Namespace: "system"}, job)).To(Succeed())
			job.Status.Succeeded = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &epiniov1alpha1.InstallEpinio{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, "Available")).To(BeTrue())
			Expect(meta.FindStatusCondition(updated.Status.Conditions, "Progressing")).To(BeNil())
		})

		It("marks the install degraded when the Job fails", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-installer-system-test-resource", Namespace: "system"}, job)).To(Succeed())
			job.Status.Failed = 1
			Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &epiniov1alpha1.InstallEpinio{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(meta.IsStatusConditionTrue(updated.Status.Conditions, "Degraded")).To(BeTrue())
			Expect(meta.FindStatusCondition(updated.Status.Conditions, "Progressing")).To(BeNil())
		})
	})
})

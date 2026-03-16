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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	epiniov1alpha1 "apps.example.com/install-epinio/api/v1alpha1"
)

var _ = Describe("InstallEpinio Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		installepinio := &epiniov1alpha1.InstallEpinio{}

		BeforeEach(func() {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "system"}})).To(SatisfyAny(
				Succeed(),
				WithTransform(errors.IsAlreadyExists, BeTrue()),
			))

			By("creating the custom resource for the Kind InstallEpinio")
			err := k8sClient.Get(ctx, typeNamespacedName, installepinio)
			if err != nil && errors.IsNotFound(err) {
				resource := &epiniov1alpha1.InstallEpinio{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
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
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &epiniov1alpha1.InstallEpinio{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance InstallEpinio")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &InstallEpinioReconciler{
				Client:              k8sClient,
				Scheme:              k8sClient.Scheme(),
				ControllerNamespace: "system",
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-installer-default-test-resource", Namespace: "system"}, job)).To(Succeed())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(job.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "EPINIO_VERSION", Value: "1.0.0"}))

			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "epinio-install-script-default-test-resource", Namespace: "system"}, configMap)).To(Succeed())

			updated := &epiniov1alpha1.InstallEpinio{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.RunningStatus).To(Equal("Installing"))
		})
	})
})

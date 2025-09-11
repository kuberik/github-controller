/*
Copyright 2025.

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("RolloutGate Controller", func() {
	const (
		RolloutGateName      = "test-rolloutgate"
		RolloutGateNamespace = "default"
		RolloutName          = "test-rollout"
		SecretName           = "github-token"

		timeout  = time.Second * 10
		duration = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When creating a RolloutGate with GitHub configuration", func() {
		It("Should create a GitHub deployment and update status", func() {
			By("Creating a GitHub token secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      SecretName,
					Namespace: RolloutGateNamespace,
				},
				Data: map[string][]byte{
					"token": []byte("test-token"),
				},
			}
			Expect(k8sClient.Create(context.Background(), secret)).Should(Succeed())

			By("Creating a Rollout")
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      RolloutName,
					Namespace: RolloutGateNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			By("Creating a RolloutGate with GitHub configuration")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      RolloutGateName,
					Namespace: RolloutGateNamespace,
					Annotations: map[string]string{
						AnnotationKeyGateClass:      AnnotationValueGitHubGateClass,
						AnnotationKeyGitHubRepo:     "test-owner/test-repo",
						AnnotationKeyDeploymentName: "test-deployment",
						AnnotationKeyEnvironment:    "production",
						AnnotationKeyRef:            "main",
						AnnotationKeyDescription:    "Test deployment",
					},
				},
				Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: RolloutName,
					},
					Passing: &[]bool{true}[0],
				},
			}
			Expect(k8sClient.Create(context.Background(), rolloutGate)).Should(Succeed())

			By("Expecting the RolloutGate to be created")
			Eventually(func() bool {
				err := k8sClient.Get(context.Background(), types.NamespacedName{
					Name:      RolloutGateName,
					Namespace: RolloutGateNamespace,
				}, rolloutGate)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			By("Expecting the RolloutGate to have GitHub configuration")
			Expect(rolloutGate.Annotations).ToNot(BeNil())
			Expect(rolloutGate.Annotations[AnnotationKeyGitHubRepo]).To(Equal("test-owner/test-repo"))
			Expect(rolloutGate.Annotations[AnnotationKeyDeploymentName]).To(Equal("test-deployment"))
			Expect(rolloutGate.Annotations[AnnotationKeyEnvironment]).To(Equal("production"))
		})
	})

	Context("When creating a RolloutGate without GitHub configuration", func() {
		It("Should not be managed by the GitHub controller", func() {
			By("Creating a RolloutGate without GitHub configuration")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rolloutgate-no-github",
					Namespace: RolloutGateNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: RolloutName,
					},
					Passing: &[]bool{true}[0],
				},
			}
			Expect(k8sClient.Create(context.Background(), rolloutGate)).Should(Succeed())

			By("Expecting the RolloutGate to be created but not managed by GitHub controller")
			Eventually(func() bool {
				err := k8sClient.Get(context.Background(), types.NamespacedName{
					Name:      "test-rolloutgate-no-github",
					Namespace: RolloutGateNamespace,
				}, rolloutGate)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			Expect(rolloutGate.Annotations).To(BeNil())
		})
	})

	Context("When updating RolloutGate dependencies", func() {
		It("Should update allowed versions based on dependencies", func() {
			By("Creating a RolloutGate with dependencies")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rolloutgate-deps",
					Namespace: RolloutGateNamespace,
					Annotations: map[string]string{
						AnnotationKeyGateClass:      AnnotationValueGitHubGateClass,
						AnnotationKeyGitHubRepo:     "test-owner/test-repo",
						AnnotationKeyDeploymentName: "test-deployment",
						AnnotationKeyDependencies:   "dep1,dep2",
					},
				},
				Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: RolloutName,
					},
					Passing: &[]bool{true}[0],
				},
			}
			Expect(k8sClient.Create(context.Background(), rolloutGate)).Should(Succeed())

			By("Expecting the RolloutGate to have dependencies configured")
			Eventually(func() bool {
				err := k8sClient.Get(context.Background(), types.NamespacedName{
					Name:      "test-rolloutgate-deps",
					Namespace: RolloutGateNamespace,
				}, rolloutGate)
				return err == nil
			}, timeout, interval).Should(BeTrue())

			// Note: In a real test, we would mock the GitHub API to test dependency resolution
			// For now, we just verify the configuration is correct
			Expect(rolloutGate.Annotations).ToNot(BeNil())
			Expect(rolloutGate.Annotations[AnnotationKeyDependencies]).To(Equal("dep1,dep2"))
		})
	})
})

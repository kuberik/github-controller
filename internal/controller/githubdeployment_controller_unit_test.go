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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberikv1alpha1 "github.com/kuberik/github-operator/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("GitHubDeployment Controller Unit Tests", func() {
	var reconciler *GitHubDeploymentReconciler

	BeforeEach(func() {
		reconciler = &GitHubDeploymentReconciler{}
	})

	Context("getCurrentVersionFromRollout", func() {
		It("Should return revision when available", func() {
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Tag:      "v1.0.0",
								Revision: &revision,
							},
							Timestamp: metav1.Now(),
						},
					},
				},
			}

			result := reconciler.getCurrentVersionFromRollout(rollout)
			Expect(result).ToNot(BeNil())
			Expect(*result).To(Equal(revision))
		})

		It("Should return nil when revision is not available", func() {
			rollout := &kuberikrolloutv1alpha1.Rollout{
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Tag: "v1.0.0",
								// Revision is nil
							},
							Timestamp: metav1.Now(),
						},
					},
				},
			}

			result := reconciler.getCurrentVersionFromRollout(rollout)
			Expect(result).To(BeNil())
		})

		It("Should return nil when history is empty", func() {
			rollout := &kuberikrolloutv1alpha1.Rollout{
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}

			result := reconciler.getCurrentVersionFromRollout(rollout)
			Expect(result).To(BeNil())
		})
	})

	Context("slicesEqual", func() {
		It("Should return true for equal slices", func() {
			a := []string{"a", "b", "c"}
			b := []string{"a", "b", "c"}
			Expect(reconciler.slicesEqual(a, b)).To(BeTrue())
		})

		It("Should return false for different slices", func() {
			a := []string{"a", "b", "c"}
			b := []string{"a", "b", "d"}
			Expect(reconciler.slicesEqual(a, b)).To(BeFalse())
		})

		It("Should return true for nil slices", func() {
			Expect(reconciler.slicesEqual(nil, nil)).To(BeTrue())
		})

		It("Should return false for one nil slice", func() {
			a := []string{"a", "b"}
			Expect(reconciler.slicesEqual(a, nil)).To(BeFalse())
			Expect(reconciler.slicesEqual(nil, a)).To(BeFalse())
		})

		It("Should return false for different lengths", func() {
			a := []string{"a", "b"}
			b := []string{"a", "b", "c"}
			Expect(reconciler.slicesEqual(a, b)).To(BeFalse())
		})
	})

	Context("GitHubDeployment validation", func() {
		It("Should validate GitHubDeployment spec", func() {
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "default",
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment",
					Environment:    "production",
					RolloutGateSpec: kuberikrolloutv1alpha1.RolloutGateSpec{
						Passing: boolPtr(true),
					},
				},
			}

			Expect(githubDeployment.Spec.RolloutRef.Name).To(Equal("test-rollout"))
			Expect(githubDeployment.Spec.Repository).To(Equal("kuberik/github-controller-testing"))
			Expect(githubDeployment.Spec.DeploymentName).To(Equal("test-deployment"))
			Expect(githubDeployment.Spec.Environment).To(Equal("production"))
			Expect(githubDeployment.Spec.RolloutGateSpec.Passing).ToNot(BeNil())
			Expect(*githubDeployment.Spec.RolloutGateSpec.Passing).To(BeTrue())
		})
	})
})

// Helper function to create bool pointer
func boolPtr(b bool) *bool {
	return &b
}

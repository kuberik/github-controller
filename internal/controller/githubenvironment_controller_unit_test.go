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
	k8sptr "k8s.io/utils/ptr"

	kuberikv1alpha1 "github.com/kuberik/environment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("GitHub Environment Controller Unit Tests", func() {
	var reconciler *GitHubEnvironmentReconciler

	BeforeEach(func() {
		reconciler = &GitHubEnvironmentReconciler{}
	})

	Context("getCurrentVersionFromRollout", func() {
		It("Should return revision when available", func() {
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
						{
							ID: k8sptr.To(int64(1)),
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
							ID: k8sptr.To(int64(1)),
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

	Context("Environment validation", func() {
		It("Should validate Environment spec", func() {
			deployment := &kuberikv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: "default",
				},
				Spec: kuberikv1alpha1.EnvironmentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout",
					},
					Name:        "test-deployment",
					Environment: "production",
					Backend: kuberikv1alpha1.BackendConfig{
						Type:    "github",
						Project: "kuberik/environment-controller-testing",
					},
				},
			}

			Expect(deployment.Spec.RolloutRef.Name).To(Equal("test-rollout"))
			Expect(deployment.Spec.Backend.Project).To(Equal("kuberik/environment-controller-testing"))
			Expect(deployment.Spec.Name).To(Equal("test-deployment"))
			Expect(deployment.Spec.Environment).To(Equal("production"))
		})
	})

	Context("Deployment Status Tracking", func() {
		Describe("deploymentStatusMap", func() {
			It("should add new entries and update existing ones", func() {
				m := newDeploymentStatusMap(nil)
				deploymentID := int64(123)

				// Add new entry
				m.set("production", "v1.0.0", "pending", &deploymentID, "https://github.com/owner/repo/deployments/123")
				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].Status).To(Equal("pending"))

				// Update existing entry
				m.set("production", "v1.0.0", "success", &deploymentID, "https://github.com/owner/repo/deployments/123")
				statuses = m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].Status).To(Equal("success"))
			})

			It("should track multiple versions and environments independently", func() {
				m := newDeploymentStatusMap(nil)

				m.set("production", "v1.0.0", "success", k8sptr.To(int64(123)), "https://github.com/owner/repo/deployments/123")
				m.set("production", "v1.1.0", "in_progress", k8sptr.To(int64(456)), "https://github.com/owner/repo/deployments/456")
				m.set("staging", "v1.0.0", "pending", k8sptr.To(int64(789)), "https://github.com/owner/repo/deployments/789")

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(3))
			})

			It("should remove entries matching the filter", func() {
				initial := []kuberikv1alpha1.EnvironmentStatusEntry{
					{Environment: "production", Version: "v1.0.0", Status: "success"},
					{Environment: "production", Version: "v1.1.0", Status: "success"},
					{Environment: "staging", Version: "v1.0.0", Status: "pending"},
				}

				m := newDeploymentStatusMap(initial)
				versionsInHistory := map[string]bool{"v1.1.0": true}
				m.remove(func(entry kuberikv1alpha1.EnvironmentStatusEntry) bool {
					return entry.Environment == "production" && !versionsInHistory[entry.Version]
				})

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(2))
			})

			It("should correctly compare with existing statuses", func() {
				existing := []kuberikv1alpha1.EnvironmentStatusEntry{
					{Environment: "production", Version: "v1.0.0", Status: "success"},
					{Environment: "staging", Version: "v1.0.0", Status: "pending"},
				}

				m := newDeploymentStatusMap(existing)
				Expect(m.equal(existing)).To(BeTrue())

				m.set("production", "v1.0.0", "failure", nil, "")
				Expect(m.equal(existing)).To(BeFalse())
			})

			It("should handle nil deployment ID and empty URL", func() {
				m := newDeploymentStatusMap(nil)
				m.set("production", "v1.0.0", "pending", nil, "")

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].DeploymentID).To(BeNil())
				Expect(statuses[0].DeploymentURL).To(BeEmpty())
			})
		})
	})
})

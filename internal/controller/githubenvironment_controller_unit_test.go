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
				revision := "v1.0.0"

				// Add new entry
				historyEntry1 := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: &deploymentID,
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &revision,
					},
					Timestamp: metav1.Now(),
				}
				m.set("production", historyEntry1)
				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].ID).ToNot(BeNil())
				Expect(*statuses[0].ID).To(Equal(deploymentID))

				// Update existing entry
				historyEntry2 := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: &deploymentID,
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &revision,
					},
					Timestamp: metav1.Now(),
				}
				m.set("production", historyEntry2)
				statuses = m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].ID).ToNot(BeNil())
				Expect(*statuses[0].ID).To(Equal(deploymentID))
			})

			It("should track multiple versions and environments independently", func() {
				m := newDeploymentStatusMap(nil)

				rev1 := "v1.0.0"
				rev2 := "v1.1.0"
				historyEntry1 := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: k8sptr.To(int64(123)),
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &rev1,
					},
					Timestamp: metav1.Now(),
				}
				historyEntry2 := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: k8sptr.To(int64(456)),
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &rev2,
					},
					Timestamp: metav1.Now(),
				}
				historyEntry3 := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: k8sptr.To(int64(789)),
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &rev1,
					},
					Timestamp: metav1.Now(),
				}

				m.set("production", historyEntry1)
				m.set("production", historyEntry2)
				m.set("staging", historyEntry3)

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(3))
			})

			It("should remove entries matching the filter", func() {
				rev1 := "v1.0.0"
				rev2 := "v1.1.0"
				initial := []kuberikv1alpha1.EnvironmentStatusEntry{
					{
						Environment: "production",
						DeploymentHistoryEntry: kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Revision: &rev1,
							},
							Timestamp: metav1.Now(),
						},
					},
					{
						Environment: "production",
						DeploymentHistoryEntry: kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Revision: &rev2,
							},
							Timestamp: metav1.Now(),
						},
					},
					{
						Environment: "staging",
						DeploymentHistoryEntry: kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Revision: &rev1,
							},
							Timestamp: metav1.Now(),
						},
					},
				}

				m := newDeploymentStatusMap(initial)
				versionsInHistory := map[string]bool{"v1.1.0": true}
				m.remove(func(entry kuberikv1alpha1.EnvironmentStatusEntry) bool {
					version := ""
					if entry.Version.Revision != nil {
						version = *entry.Version.Revision
					}
					return entry.Environment == "production" && !versionsInHistory[version]
				})

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(2))
			})

			It("should correctly compare with existing statuses", func() {
				rev1 := "v1.0.0"
				id1 := int64(1)
				id2 := int64(2)
				existing := []kuberikv1alpha1.EnvironmentStatusEntry{
					{
						Environment: "production",
						DeploymentHistoryEntry: kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							ID: &id1,
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Revision: &rev1,
							},
							Timestamp: metav1.Now(),
						},
					},
					{
						Environment: "staging",
						DeploymentHistoryEntry: kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							ID: &id2,
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Revision: &rev1,
							},
							Timestamp: metav1.Now(),
						},
					},
				}

				m := newDeploymentStatusMap(existing)
				Expect(m.equal(existing)).To(BeTrue())

				id3 := int64(3)
				historyEntry := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: &id3,
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &rev1,
					},
					Timestamp: metav1.Now(),
				}
				m.set("production", historyEntry)
				Expect(m.equal(existing)).To(BeFalse())
			})

			It("should handle nil deployment ID", func() {
				m := newDeploymentStatusMap(nil)
				rev := "v1.0.0"
				historyEntry := &kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					ID: nil,
					Version: kuberikrolloutv1alpha1.VersionInfo{
						Revision: &rev,
					},
					Timestamp: metav1.Now(),
				}
				m.set("production", historyEntry)

				statuses := m.toSlice()
				Expect(statuses).To(HaveLen(1))
				Expect(statuses[0].ID).To(BeNil())
			})
		})
	})
})

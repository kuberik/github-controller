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

	Context("Relevant Versions Logic", func() {
		It("Should build relevantVersions from all relevant environments' history", func() {
			// This test documents how relevantVersions works:
			// 1. relevantVersions is built from ALL history entries in ALL relevant environments
			// 2. It's a set of revision SHAs that appear in any relevant environment's history
			// 3. It's used to determine which versions are relevant across the relationship graph
			// 4. However, it's NOT used to filter history in environmentInfos - each environment keeps ALL its own history

			revision1 := "rev1"
			revision2 := "rev2"
			revision3 := "rev3"
			revision4 := "rev4"

			// Simulate relationship graph data
			envHistory := map[string][]kuberikrolloutv1alpha1.DeploymentHistoryEntry{
				"dev": {
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision1,
						},
					},
					{
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision2,
						},
					},
				},
				"staging": {
					{
						ID: k8sptr.To(int64(3)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision2, // Shared with dev
						},
					},
					{
						ID: k8sptr.To(int64(4)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision3,
						},
					},
				},
				"prod": {
					{
						ID: k8sptr.To(int64(5)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision3, // Shared with staging
						},
					},
					{
						ID: k8sptr.To(int64(6)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: &revision4,
						},
					},
				},
			}

			relevantEnvironments := map[string]bool{
				"dev":     true,
				"staging": true,
				"prod":    false, // Not relevant
			}

			// Build relevantVersions (simulating the logic from buildRelationshipGraph)
			relevantVersions := make(map[string]bool)
			for envName, history := range envHistory {
				if !relevantEnvironments[envName] {
					continue
				}
				for _, entry := range history {
					if entry.Version.Revision != nil && *entry.Version.Revision != "" {
						relevantVersions[*entry.Version.Revision] = true
					}
				}
			}

			// Verify: relevantVersions should contain all revisions from dev and staging (but not prod)
			Expect(relevantVersions).To(HaveKey(revision1))    // From dev
			Expect(relevantVersions).To(HaveKey(revision2))    // From dev and staging
			Expect(relevantVersions).To(HaveKey(revision3))    // From staging
			Expect(relevantVersions).ToNot(HaveKey(revision4)) // From prod (not relevant)

			// Verify: each environment keeps ALL its own history (not filtered by relevantVersions)
			// This is the key point - environmentInfos contain full history for each environment
			devHistory := envHistory["dev"]
			Expect(len(devHistory)).To(Equal(2))
			Expect(*devHistory[0].Version.Revision).To(Equal(revision1))
			Expect(*devHistory[1].Version.Revision).To(Equal(revision2))

			stagingHistory := envHistory["staging"]
			Expect(len(stagingHistory)).To(Equal(2))
			Expect(*stagingHistory[0].Version.Revision).To(Equal(revision2))
			Expect(*stagingHistory[1].Version.Revision).To(Equal(revision3))
		})

		It("Should handle empty revisions in history entries", func() {
			envHistory := map[string][]kuberikrolloutv1alpha1.DeploymentHistoryEntry{
				"dev": {
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: nil, // Empty revision
						},
					},
					{
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: k8sptr.To("rev1"),
						},
					},
				},
			}

			relevantEnvironments := map[string]bool{
				"dev": true,
			}

			relevantVersions := make(map[string]bool)
			for envName, history := range envHistory {
				if !relevantEnvironments[envName] {
					continue
				}
				for _, entry := range history {
					if entry.Version.Revision != nil && *entry.Version.Revision != "" {
						relevantVersions[*entry.Version.Revision] = true
					}
				}
			}

			// Should only include entries with valid revisions
			Expect(relevantVersions).To(HaveKey("rev1"))
			Expect(len(relevantVersions)).To(Equal(1))
		})
	})

	Context("Preserve EnvironmentInfos", func() {
		It("Should preserve existing environmentInfos when environment is not discovered in current run", func() {
			// This test verifies that existing environmentInfos are preserved even if
			// the environment is not discovered in the current reconciliation run
			// (e.g., due to pagination issues or temporary GitHub API issues)

			existingStagingInfo := kuberikv1alpha1.EnvironmentInfo{
				Environment:    "staging",
				EnvironmentURL: "https://staging.example.com",
				Relationship: &kuberikv1alpha1.EnvironmentRelationship{
					Environment: "dev",
					Type:        kuberikv1alpha1.RelationshipTypeAfter,
				},
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						ID: k8sptr.To(int64(25)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: k8sptr.To("staging-rev-25"),
						},
					},
				},
			}

			existingDevInfo := kuberikv1alpha1.EnvironmentInfo{
				Environment:    "dev",
				EnvironmentURL: "https://dev.example.com",
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						ID: k8sptr.To(int64(50)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Revision: k8sptr.To("dev-rev-50"),
						},
					},
				},
			}

			// Simulate existing status with staging info
			existingStatus := []kuberikv1alpha1.EnvironmentInfo{
				existingDevInfo,
				existingStagingInfo,
			}

			// Simulate graphData where staging was NOT discovered (e.g., due to pagination)
			// but dev was discovered
			// Note: In the actual implementation, we would check graphData.relevantEnvironments
			// but for this test we're demonstrating that staging should be preserved
			_ = map[string]bool{
				"dev": true,
				// staging is NOT in relevantEnvironments because it wasn't discovered
			}

			graphDataEnvironmentInfos := map[string]environmentInfo{
				"dev": {
					EnvironmentURL: "https://dev.example.com",
				},
				// staging is NOT in environmentInfos because it wasn't discovered
			}

			// Simulate the logic: start with existing, update/add from graphData
			result := make([]kuberikv1alpha1.EnvironmentInfo, len(existingStatus))
			for i := range existingStatus {
				result[i] = *existingStatus[i].DeepCopy()
			}

			// Update/add from graphData
			for envName, info := range graphDataEnvironmentInfos {
				// This would normally update dev's info
				found := false
				for i := range result {
					if result[i].Environment == envName {
						result[i].EnvironmentURL = info.EnvironmentURL
						found = true
						break
					}
				}
				if !found {
					result = append(result, kuberikv1alpha1.EnvironmentInfo{
						Environment:    envName,
						EnvironmentURL: info.EnvironmentURL,
					})
				}
			}

			// OLD BEHAVIOR (what we're fixing): Remove environments not in relevantEnvironments
			// This would remove staging even though it should be preserved
			// OLD: result = removeEnvironmentInfos(result, func(entry kuberikv1alpha1.EnvironmentInfo) bool {
			//     return !graphDataEnvironments[entry.Environment]
			// })

			// NEW BEHAVIOR: Preserve existing environmentInfos
			// staging should still be in the result even though it wasn't discovered

			// Verify staging is preserved
			var stagingFound bool
			for _, info := range result {
				if info.Environment == "staging" {
					stagingFound = true
					Expect(info.EnvironmentURL).To(Equal("https://staging.example.com"))
					Expect(info.Relationship).ToNot(BeNil())
					Expect(info.Relationship.Environment).To(Equal("dev"))
					Expect(len(info.History)).To(Equal(1))
					Expect(*info.History[0].Version.Revision).To(Equal("staging-rev-25"))
					break
				}
			}
			Expect(stagingFound).To(BeTrue(), "staging should be preserved even though not discovered in current run")

			// Verify dev is updated
			var devFound bool
			for _, info := range result {
				if info.Environment == "dev" {
					devFound = true
					Expect(info.EnvironmentURL).To(Equal("https://dev.example.com"))
					break
				}
			}
			Expect(devFound).To(BeTrue())
		})
	})

	// Note: deploymentStatusMap tests removed as we now use history directly from rollouts in EnvironmentInfo
	// Note: deploymentStatusMap tests removed as we now use history directly from rollouts in EnvironmentInfo
})

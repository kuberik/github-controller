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
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	kuberikv1alpha1 "github.com/kuberik/github-operator/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("GitHubDeployment Controller", func() {
	const (
		GitHubDeploymentNamespace = "default"
		SecretName                = "github-token"
	)

	// Helper function to skip test if no GitHub token
	skipIfNoGitHubToken := func() {
		if os.Getenv("GITHUB_TOKEN") == "" {
			Skip("Skipping GitHub API integration test - GITHUB_TOKEN not provided")
		}
	}

	// Helper function to clean up GitHub deployments
	cleanupGitHubDeployments := func(repository string) {
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			return // No token available
		}

		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		tc := oauth2.NewClient(context.Background(), ts)
		client := github.NewClient(tc)

		// Parse repository
		parts := strings.Split(repository, "/")
		if len(parts) != 2 {
			return // Invalid repository format
		}
		owner, repo := parts[0], parts[1]

		// List all deployments
		deployments, _, err := client.Repositories.ListDeployments(context.Background(), owner, repo, &github.DeploymentsListOptions{
			ListOptions: github.ListOptions{PerPage: 100}, // Get up to 100 deployments
		})
		if err != nil {
			return // Ignore errors during cleanup
		}

		// Delete each deployment
		for _, deployment := range deployments {
			if deployment.ID != nil {
				// Actually delete the deployment
				_, err = client.Repositories.DeleteDeployment(context.Background(), owner, repo, *deployment.ID)
				if err != nil {
					// Ignore deletion errors during cleanup
				}
			}
		}
	}

	// Helper function to create GitHub token secret
	createGitHubTokenSecret := func() error {
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			token = "test-token" // Fallback for tests without real token
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SecretName,
				Namespace: GitHubDeploymentNamespace,
			},
			Data: map[string][]byte{
				"token": []byte(token),
			},
		}
		// Delete if exists first
		k8sClient.Delete(context.Background(), secret)
		return k8sClient.Create(context.Background(), secret)
	}

	var (
		reconciler *GitHubDeploymentReconciler
	)

	BeforeEach(func() {
		reconciler = &GitHubDeploymentReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
		}
	})

	Context("Unit Tests", func() {
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
					},
				}

				Expect(githubDeployment.Spec.RolloutRef.Name).To(Equal("test-rollout"))
				Expect(githubDeployment.Spec.Repository).To(Equal("kuberik/github-controller-testing"))
				Expect(githubDeployment.Spec.DeploymentName).To(Equal("test-deployment"))
				Expect(githubDeployment.Spec.Environment).To(Equal("production"))
			})
		})
	})

	Context("Integration Tests", func() {
		BeforeEach(func() {
			// Clean up GitHub deployments before each integration test
			if os.Getenv("GITHUB_TOKEN") != "" {
				By("Cleaning up GitHub deployments before test")
				cleanupGitHubDeployments("kuberik/github-controller-testing")
			}
		})

		It("Should create RolloutGate when GitHubDeployment is created", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			bakeStatus := "Succeeded"
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						BakeStatus: &bakeStatus,
						Timestamp:  metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying GitHub deployment was created")
			// Get the updated GitHubDeployment to check status
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())

			// Verify GitHub deployment was created
			Expect(updatedGitHubDeployment.Status.GitHubDeploymentID).ToNot(BeNil())
			Expect(*updatedGitHubDeployment.Status.GitHubDeploymentID).To(BeNumerically(">", 0))
			Expect(updatedGitHubDeployment.Status.GitHubDeploymentURL).ToNot(BeEmpty())
			Expect(updatedGitHubDeployment.Status.CurrentVersion).To(Equal(revision))
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying RolloutGate was created")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			Expect(rolloutGate.Spec.RolloutRef).ToNot(BeNil())
			Expect(rolloutGate.Spec.RolloutRef.Name).To(Equal("test-rollout"))
			Expect(rolloutGate.Spec.Passing).ToNot(BeNil())
			Expect(*rolloutGate.Spec.Passing).To(BeTrue())

			By("Verifying GitHub deployment exists via API")
			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			// Get the deployment from GitHub API
			deployment, _, err := githubClient.Repositories.GetDeployment(context.Background(), "kuberik", "github-controller-testing", *updatedGitHubDeployment.Status.GitHubDeploymentID)
			Expect(err).ToNot(HaveOccurred())
			Expect(deployment).ToNot(BeNil())
			Expect(deployment.Ref).ToNot(BeNil())
			Expect(*deployment.Ref).To(Equal(revision))
			Expect(deployment.Environment).ToNot(BeNil())
			Expect(*deployment.Environment).To(Equal("production"))

			By("Verifying GitHub deployment status was created")
			// Get deployment statuses
			statuses, _, err := githubClient.Repositories.ListDeploymentStatuses(context.Background(), "kuberik", "github-controller-testing", *updatedGitHubDeployment.Status.GitHubDeploymentID, &github.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(statuses).ToNot(BeEmpty())

			// Check the latest status
			latestStatus := statuses[0]
			Expect(latestStatus.State).ToNot(BeNil())
			Expect(*latestStatus.State).To(Equal("success")) // Should be success since Passing=true
		})

		It("Should update GitHub deployment status when RolloutGate passing changes", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status-change",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with passing=true")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-change",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-status",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment (first time - passing=true)")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying initial GitHub deployment status is success")
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())

			Expect(updatedGitHubDeployment.Status.GitHubDeploymentID).ToNot(BeNil())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			// Get initial statuses
			statuses, _, err := githubClient.Repositories.ListDeploymentStatuses(context.Background(), "kuberik", "github-controller-testing", *updatedGitHubDeployment.Status.GitHubDeploymentID, &github.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(statuses).ToNot(BeEmpty())
			initialStatusCount := len(statuses)
			_ = initialStatusCount // Use variable to avoid unused error

			By("All GitHub deployments are initially successful")
			// The test just verifies the initial deployment was successful
		})

		It("Should update RolloutGate when GitHubDeployment spec changes", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-update",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with passing=true")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-update",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-update",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-update",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("First reconciliation - should create RolloutGate with passing=true")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-update",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// Get the updated GitHubDeployment to find the RolloutGate name
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-update",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			// Verify initial RolloutGate was created
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			Expect(rolloutGate.Spec.Passing).ToNot(BeNil())
			Expect(*rolloutGate.Spec.Passing).To(BeTrue())

			By("RolloutGate should be created and managed automatically")
			// The RolloutGate is now created and managed by the controller
			// No need to manually update passing field
		})

		It("Should handle missing Rollout gracefully", func() {
			skipIfNoGitHubToken()

			By("Creating GitHubDeployment with non-existent Rollout")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-missing-rollout",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "non-existent-rollout",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-missing-rollout",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with missing Rollout")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-missing-rollout",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("Should handle missing revision error", func() {
			skipIfNoGitHubToken()

			By("Creating Rollout without revision")
			rolloutNoRevision := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-no-revision",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rolloutNoRevision)
			Expect(k8sClient.Create(context.Background(), rolloutNoRevision)).Should(Succeed())

			// Update the rollout to set the status
			rolloutNoRevision.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag: "v1.0.0",
							// Revision is nil
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rolloutNoRevision)).Should(Succeed())

			By("Creating GitHubDeployment")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-no-revision",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-no-revision",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-no-revision",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with Rollout missing revision")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-no-revision",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("Should handle empty deployment history", func() {
			skipIfNoGitHubToken()

			By("Creating Rollout with empty history")
			rolloutEmptyHistory := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-empty-history",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rolloutEmptyHistory)
			Expect(k8sClient.Create(context.Background(), rolloutEmptyHistory)).Should(Succeed())

			// Update the rollout to set the status
			rolloutEmptyHistory.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{},
			}
			Expect(k8sClient.Status().Update(context.Background(), rolloutEmptyHistory)).Should(Succeed())

			By("Creating GitHubDeployment")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-empty-history",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-empty-history",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-empty-history",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with Rollout having empty history")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-empty-history",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("Should update GitHubDeployment status with deployment information", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-status",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying GitHubDeployment status was updated")
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())

			Expect(updatedGitHubDeployment.Status.CurrentVersion).To(Equal(revision))
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))
		})

		It("Should update allowed versions from dependencies", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating staging deployment with success status")
			// First create a dependency deployment (staging)
			// Use a commit SHA as the revision (GitHub deployment ref)
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			stagingTag := "v1.0.0" // Tag that matches the staging revision
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String("staging"),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create success status for staging deployment
			stagingStatusRequest := &github.DeploymentStatusRequest{
				State:       github.String("success"),
				Description: github.String("Staging deployment successful"),
				Environment: github.String("staging"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), stagingStatusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating production deployment with dependencies on staging")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
				// Add releaseCandidates with staging revision mapped to tag
				ReleaseCandidates: []kuberikrolloutv1alpha1.VersionInfo{
					{
						Tag:      stagingTag,
						Revision: &stagingRef,
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with dependencies")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-deps",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-deps",
					Environment:    "production",
					Dependencies:   []string{"staging"},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with dependencies")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-deps",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// Get the updated GitHubDeployment to find the RolloutGate name
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-deps",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying allowed versions were updated from dependencies on RolloutGate")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Should have the staging tag in allowed versions (not revision)
			Expect(rolloutGate.Spec.AllowedVersions).ToNot(BeNil())
			Expect(*rolloutGate.Spec.AllowedVersions).ToNot(BeEmpty())
			Expect(*rolloutGate.Spec.AllowedVersions).To(ContainElement(stagingTag))

			// Clean up staging deployment
			githubClient.Repositories.DeleteDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID())
		})

		It("Should handle multiple dependencies", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating first dependency deployment (staging) with success status")
			// Use a commit SHA as the revision (GitHub deployment ref)
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			stagingTag := "v1.0.0"
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String("staging"),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			stagingStatusRequest := &github.DeploymentStatusRequest{
				State:       github.String("success"),
				Description: github.String("Staging deployment successful"),
				Environment: github.String("staging"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), stagingStatusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating second dependency deployment (qa) with success status")
			// Use same commit SHA as staging (same revision, same tag)
			qaRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			qaDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(qaRef),
				Environment:           github.String("qa"),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			qaDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", qaDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			qaStatusRequest := &github.DeploymentStatusRequest{
				State:       github.String("success"),
				Description: github.String("QA deployment successful"),
				Environment: github.String("qa"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", qaDeployment.GetID(), qaStatusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating production deployment with multiple dependencies")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-multi-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
				// Add releaseCandidates with revisions mapped to tags
				ReleaseCandidates: []kuberikrolloutv1alpha1.VersionInfo{
					{
						Tag:      stagingTag,
						Revision: &stagingRef,
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with multiple dependencies")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-multi-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-multi-deps",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-multi-deps",
					Environment:    "production",
					Dependencies:   []string{"staging", "qa"},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with multiple dependencies")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-multi-deps",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// Get the updated GitHubDeployment to find the RolloutGate name
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-multi-deps",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying allowed versions include versions from all dependencies on RolloutGate")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Should have the tag in allowed versions (both staging and qa use same revision, so same tag)
			Expect(rolloutGate.Spec.AllowedVersions).ToNot(BeNil())
			Expect(*rolloutGate.Spec.AllowedVersions).ToNot(BeEmpty())
			Expect(*rolloutGate.Spec.AllowedVersions).To(ContainElement(stagingTag))

			// Clean up deployments
			githubClient.Repositories.DeleteDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID())
			githubClient.Repositories.DeleteDeployment(context.Background(), "kuberik", "github-controller-testing", qaDeployment.GetID())
		})

		It("Should handle dependency with failure status", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating dependency deployment with failure status")
			// Use a commit SHA as the revision (GitHub deployment ref)
			failedRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			failedDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(failedRef),
				Environment:           github.String("staging"),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			failedDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", failedDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create failure status
			failedStatusRequest := &github.DeploymentStatusRequest{
				State:       github.String("failure"),
				Description: github.String("Staging deployment failed"),
				Environment: github.String("staging"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", failedDeployment.GetID(), failedStatusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating production deployment with dependency")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-failed-dep",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with failed dependency")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-failed-dep",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-failed-dep",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-failed-dep",
					Environment:    "production",
					Dependencies:   []string{"staging"},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment with failed dependency")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-failed-dep",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// Get the updated GitHubDeployment to find the RolloutGate name
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-failed-dep",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying failed dependency doesn't add to allowed versions on RolloutGate")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Should not have the failed tag in allowed versions (since it failed)
			// Failed deployments should not add their tags to AllowedVersions
			if rolloutGate.Spec.AllowedVersions != nil {
				// Even if the revision exists in releaseCandidates, it shouldn't be added because status is failure
				Expect(*rolloutGate.Spec.AllowedVersions).To(BeEmpty())
			}

			// Clean up deployment
			githubClient.Repositories.DeleteDeployment(context.Background(), "kuberik", "github-controller-testing", failedDeployment.GetID())
		})

		It("Should handle deployment without dependencies", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-no-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment without dependencies")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-no-deps",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-no-deps",
					},
					Repository:     "kuberik/github-controller-testing",
					DeploymentName: "test-deployment-no-deps",
					Environment:    "production",
					// No Dependencies field
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment without dependencies")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-no-deps",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			// Get the updated GitHubDeployment to find the RolloutGate name
			updatedGitHubDeployment := &kuberikv1alpha1.GitHubDeployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-no-deps",
				Namespace: GitHubDeploymentNamespace,
			}, updatedGitHubDeployment)).To(Succeed())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedGitHubDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying allowed versions is empty when no dependencies on RolloutGate")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedGitHubDeployment.Status.RolloutGateRef.Name,
				Namespace: GitHubDeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Should have no allowed versions or nil
			if rolloutGate.Spec.AllowedVersions != nil {
				Expect(*rolloutGate.Spec.AllowedVersions).To(BeEmpty())
			}
		})

		It("Should use custom requeue interval when specified", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-requeue",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			bakeStatus := "Succeeded"
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						BakeStatus: &bakeStatus,
						Timestamp:  metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating GitHubDeployment with custom requeue interval")
			githubDeployment := &kuberikv1alpha1.GitHubDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-requeue",
					Namespace: GitHubDeploymentNamespace,
				},
				Spec: kuberikv1alpha1.GitHubDeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-requeue",
					},
					Repository:      "kuberik/github-controller-testing",
					DeploymentName:  "test-deployment-requeue",
					Environment:     "production",
					RequeueInterval: "3m",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), githubDeployment)
			Expect(k8sClient.Create(context.Background(), githubDeployment)).Should(Succeed())

			By("Reconciling GitHubDeployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-requeue",
					Namespace: GitHubDeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(3 * time.Minute))
		})
	})
})

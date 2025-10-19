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
	"fmt"
	"os"
	"time"

	"github.com/google/go-github/v62/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

type MockGitHubClient struct {
	deployments        map[string]*github.Deployment
	statuses           map[int64][]*github.DeploymentStatus
	createdDeployments []*github.DeploymentRequest
	updatedDeployments []*github.DeploymentRequest
}

func NewMockGitHubClient() *MockGitHubClient {
	return &MockGitHubClient{
		deployments:        make(map[string]*github.Deployment),
		statuses:           make(map[int64][]*github.DeploymentStatus),
		createdDeployments: []*github.DeploymentRequest{},
		updatedDeployments: []*github.DeploymentRequest{},
	}
}

func (m *MockGitHubClient) CreateDeployment(ctx context.Context, owner, repo string, request *github.DeploymentRequest) (*github.Deployment, *github.Response, error) {
	m.createdDeployments = append(m.createdDeployments, request)

	deploymentID := int64(len(m.deployments) + 1)
	deployment := &github.Deployment{
		ID:          &deploymentID,
		URL:         github.String(fmt.Sprintf("https://api.github.com/repos/%s/%s/deployments/%d", owner, repo, deploymentID)),
		Ref:         request.Ref,
		Environment: request.Environment,
		Description: request.Description,
	}

	key := fmt.Sprintf("%s/%s/%s", owner, repo, *request.Environment)
	m.deployments[key] = deployment

	return deployment, &github.Response{}, nil
}

func (m *MockGitHubClient) UpdateDeployment(ctx context.Context, owner, repo string, deploymentID int64, request *github.DeploymentRequest) (*github.Deployment, *github.Response, error) {
	m.updatedDeployments = append(m.updatedDeployments, request)

	deployment := &github.Deployment{
		ID:          &deploymentID,
		URL:         github.String(fmt.Sprintf("https://api.github.com/repos/%s/%s/deployments/%d", owner, repo, deploymentID)),
		Ref:         request.Ref,
		Environment: request.Environment,
		Description: request.Description,
	}

	return deployment, &github.Response{}, nil
}

func (m *MockGitHubClient) ListDeployments(ctx context.Context, owner, repo string, opt *github.DeploymentsListOptions) ([]*github.Deployment, *github.Response, error) {
	var deployments []*github.Deployment
	for _, dep := range m.deployments {
		if dep.Environment != nil && *dep.Environment == opt.Environment {
			deployments = append(deployments, dep)
		}
	}
	return deployments, &github.Response{}, nil
}

func (m *MockGitHubClient) ListDeploymentStatuses(ctx context.Context, owner, repo string, deployment int64, opt *github.ListOptions) ([]*github.DeploymentStatus, *github.Response, error) {
	if statuses, exists := m.statuses[deployment]; exists {
		return statuses, &github.Response{}, nil
	}
	return []*github.DeploymentStatus{}, &github.Response{}, nil
}

func (m *MockGitHubClient) SetDeploymentStatuses(deploymentID int64, statuses []*github.DeploymentStatus) {
	m.statuses[deploymentID] = statuses
}

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

	// Helper function to create GitHub token secret with environment variable
	createGitHubTokenSecret := func(k8sClient client.Client, namespace string) error {
		githubToken := os.Getenv("GITHUB_TOKEN")
		if githubToken == "" {
			return fmt.Errorf("GITHUB_TOKEN environment variable not set")
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SecretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				"token": []byte(githubToken),
			},
		}

		// Delete if exists first
		k8sClient.Delete(context.Background(), secret)
		return k8sClient.Create(context.Background(), secret)
	}

	// Helper function to skip test if no GitHub token
	skipIfNoGitHubToken := func() {
		if os.Getenv("GITHUB_TOKEN") == "" {
			Skip("Skipping GitHub API integration test - GITHUB_TOKEN not provided")
		}
	}

	var (
		reconciler *RolloutGateReconciler
	)

	BeforeEach(func() {
		reconciler = &RolloutGateReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
	})

	Context("Reconcile method tests", func() {
		BeforeEach(func() {
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), secret)
			Expect(k8sClient.Create(context.Background(), secret)).Should(Succeed())

			By("Creating a Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
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
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
						{
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Tag:      "v1.0.0",
								Revision: &revision,
							},
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
		})

		It("Should create GitHub deployment and update annotations", func() {
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

			// Test the helper methods directly
			By("Testing isGitHubGate helper")
			Expect(reconciler.isGitHubGate(rolloutGate)).To(BeTrue())

			By("Testing getGitHubConfig helper")
			config, err := reconciler.getGitHubConfig(context.Background(), rolloutGate)
			Expect(err).ToNot(HaveOccurred())
			Expect(config.Repository).To(Equal("test-owner/test-repo"))
			Expect(config.DeploymentName).To(Equal("test-deployment"))
			Expect(config.Environment).To(Equal("production"))

			By("Testing getCurrentVersionFromRollout helper")
			rollout := &kuberikrolloutv1alpha1.Rollout{}
			err = k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      RolloutName,
				Namespace: RolloutGateNamespace,
			}, rollout)
			Expect(err).ToNot(HaveOccurred())

			version := reconciler.getCurrentVersionFromRollout(rollout)
			Expect(version).ToNot(BeNil())
			Expect(*version).To(Equal("0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"))
		})

		It("Should handle missing revision gracefully", func() {
			By("Creating a Rollout without revision")
			rolloutNoRevision := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-no-revision",
					Namespace: RolloutGateNamespace,
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
						},
					},
				},
			}
			Expect(k8sClient.Create(context.Background(), rolloutNoRevision)).Should(Succeed())

			By("Testing getCurrentVersionFromRollout with nil revision")
			version := reconciler.getCurrentVersionFromRollout(rolloutNoRevision)
			Expect(version).To(BeNil())
		})

		It("Should handle empty deployment history", func() {
			By("Creating a Rollout with empty history")
			rolloutEmptyHistory := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-empty-history",
					Namespace: RolloutGateNamespace,
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
			Expect(k8sClient.Create(context.Background(), rolloutEmptyHistory)).Should(Succeed())

			By("Testing getCurrentVersionFromRollout with empty history")
			version := reconciler.getCurrentVersionFromRollout(rolloutEmptyHistory)
			Expect(version).To(BeNil())
		})
	})

	Context("Non-GitHub RolloutGate tests", func() {
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

			By("Testing isGitHubGate helper")
			Expect(reconciler.isGitHubGate(rolloutGate)).To(BeFalse())
		})
	})

	Context("Full reconciliation test", func() {
		BeforeEach(func() {
			skipIfNoGitHubToken()
			Expect(createGitHubTokenSecret(k8sClient, RolloutGateNamespace)).To(Succeed())
		})
		It("Should perform complete reconciliation flow", func() {
			By("Creating a RolloutGate with GitHub configuration")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-full-reconcile",
					Namespace: RolloutGateNamespace,
					Annotations: map[string]string{
						AnnotationKeyGateClass:      AnnotationValueGitHubGateClass,
						AnnotationKeyGitHubRepo:     "kuberik/github-controller-testing",
						AnnotationKeyDeploymentName: "test-deployment",
						AnnotationKeyEnvironment:    "production",
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

			By("Calling Reconcile method")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-full-reconcile",
					Namespace: RolloutGateNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			By("Verifying RolloutGate was updated with GitHub deployment info")
			updatedRolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			err = k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-full-reconcile",
				Namespace: RolloutGateNamespace,
			}, updatedRolloutGate)
			Expect(err).ToNot(HaveOccurred())

			// Verify that GitHub deployment ID and URL were stored in annotations
			Expect(updatedRolloutGate.Annotations).ToNot(BeNil())
			Expect(updatedRolloutGate.Annotations["kuberik.com/github-deployment-id"]).ToNot(BeEmpty())
			Expect(updatedRolloutGate.Annotations["kuberik.com/github-deployment-url"]).ToNot(BeEmpty())
			Expect(updatedRolloutGate.Annotations["kuberik.com/github-last-sync"]).ToNot(BeEmpty())
		})

		It("Should handle missing revision error", func() {
			skipIfNoGitHubToken()

			By("Creating a Rollout without revision")
			rolloutNoRevision := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-no-revision",
					Namespace: RolloutGateNamespace,
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

			By("Creating a RolloutGate referencing the rollout without revision")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-no-revision",
					Namespace: RolloutGateNamespace,
					Annotations: map[string]string{
						AnnotationKeyGateClass:      AnnotationValueGitHubGateClass,
						AnnotationKeyGitHubRepo:     "test-owner/test-repo",
						AnnotationKeyDeploymentName: "test-deployment",
						AnnotationKeyEnvironment:    "production",
					},
				},
				Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
					RolloutRef: &corev1.LocalObjectReference{
						Name: "test-rollout-no-revision",
					},
					Passing: &[]bool{true}[0],
				},
			}
			Expect(k8sClient.Create(context.Background(), rolloutGate)).Should(Succeed())

			By("Calling Reconcile method - should return error")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-no-revision",
					Namespace: RolloutGateNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no revision found in rollout history"))
			Expect(result.RequeueAfter).To(Equal(time.Duration(0))) // Error returns empty result
		})

		It("Should skip non-GitHub RolloutGates", func() {
			By("Creating a RolloutGate without GitHub configuration")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-non-github",
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

			By("Calling Reconcile method - should skip")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-non-github",
					Namespace: RolloutGateNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			By("Verifying no GitHub annotations were added")
			updatedRolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			err = k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-non-github",
				Namespace: RolloutGateNamespace,
			}, updatedRolloutGate)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedRolloutGate.Annotations).To(BeNil())
		})
	})
})

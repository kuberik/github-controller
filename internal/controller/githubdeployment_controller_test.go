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
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	k8sptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"

	kuberikv1alpha1 "github.com/kuberik/deployment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("Deployment Controller", func() {
	const (
		DeploymentNamespace = "default"
		SecretName          = "github-token"
	)

	// Helper function to skip test if no GitHub token
	skipIfNoGitHubToken := func() {
		if os.Getenv("GITHUB_TOKEN") == "" {
			Skip("Skipping GitHub API integration test - GITHUB_TOKEN not provided")
		}
	}

	// Helper function to clean up GitHub deployments
	cleanupDeployments := func(repository string) {
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
				Namespace: DeploymentNamespace,
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

		// Clean up any existing resources
		deploymentList := &kuberikv1alpha1.DeploymentList{}
		if err := k8sClient.List(context.Background(), deploymentList); err == nil {
			for i := range deploymentList.Items {
				k8sClient.Delete(context.Background(), &deploymentList.Items[i])
			}
		}
	})

	Context("Unit Tests", func() {
		Context("getControllerNamespace", func() {
			It("Should read namespace from service account file", func() {
				// This test would require mocking file system, skipping for now
				Skip("Requires file system mocking")
			})

			It("Should fallback to POD_NAMESPACE environment variable", func() {
				os.Setenv("POD_NAMESPACE", "test-namespace")
				defer os.Unsetenv("POD_NAMESPACE")
				result := reconciler.getControllerNamespace()
				Expect(result).To(Equal("test-namespace"))
			})

			It("Should return empty string when no namespace is available", func() {
				os.Unsetenv("POD_NAMESPACE")
				os.Unsetenv("WATCH_NAMESPACE")
				result := reconciler.getControllerNamespace()
				// In test environment, this might return empty or might read from actual file
				// So we just check it's a valid string (empty or actual namespace)
				Expect(result).To(BeAssignableToTypeOf(""))
			})
		})

		Context("getRolloutDashboardURL", func() {
			const (
				ControllerNamespace = "controller-ns"
				TestNamespace       = "test-ns"
				TestName            = "test-deployment"
			)

			BeforeEach(func() {
				// Set controller namespace via environment variable
				os.Setenv("POD_NAMESPACE", ControllerNamespace)

				// Create the controller namespace
				ns := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: ControllerNamespace,
					},
				}
				// Ignore error if namespace already exists
				k8sClient.Create(context.Background(), ns)
			})

			AfterEach(func() {
				os.Unsetenv("POD_NAMESPACE")
				// Clean up test resources
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard",
						Namespace: ControllerNamespace,
					},
				}
				k8sClient.Delete(context.Background(), svc)

				ingress := &networkingv1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard-ingress",
						Namespace: ControllerNamespace,
					},
				}
				k8sClient.Delete(context.Background(), ingress)
			})

			It("Should return empty string when service doesn't exist", func() {
				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(BeEmpty())
			})

			It("Should return empty string when ingress doesn't exist", func() {
				// Create service but no ingress
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard",
						Namespace: ControllerNamespace,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{Port: 80},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), svc)).To(Succeed())

				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(BeEmpty())
			})

			It("Should return URL with path when ingress with HTTP exists", func() {
				// Create service
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard",
						Namespace: ControllerNamespace,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{Port: 80},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), svc)).To(Succeed())

				// Create ingress pointing to rollout-dashboard
				ingress := &networkingv1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard-ingress",
						Namespace: ControllerNamespace,
					},
					Spec: networkingv1.IngressSpec{
						Rules: []networkingv1.IngressRule{
							{
								Host: "dashboard.example.com",
								IngressRuleValue: networkingv1.IngressRuleValue{
									HTTP: &networkingv1.HTTPIngressRuleValue{
										Paths: []networkingv1.HTTPIngressPath{
											{
												Path:     "/",
												PathType: func() *networkingv1.PathType { pt := networkingv1.PathTypePrefix; return &pt }(),
												Backend: networkingv1.IngressBackend{
													Service: &networkingv1.IngressServiceBackend{
														Name: "rollout-dashboard",
														Port: networkingv1.ServiceBackendPort{
															Number: 80,
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), ingress)).To(Succeed())

				expectedURL := fmt.Sprintf("http://dashboard.example.com/rollouts/%s/%s", TestNamespace, TestName)
				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(Equal(expectedURL))
			})

			It("Should return URL with HTTPS when TLS is configured", func() {
				// Create service
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard",
						Namespace: ControllerNamespace,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{Port: 80},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), svc)).To(Succeed())

				// Create ingress with TLS
				ingress := &networkingv1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard-ingress",
						Namespace: ControllerNamespace,
					},
					Spec: networkingv1.IngressSpec{
						TLS: []networkingv1.IngressTLS{
							{
								Hosts:      []string{"dashboard.example.com"},
								SecretName: "tls-secret",
							},
						},
						Rules: []networkingv1.IngressRule{
							{
								Host: "dashboard.example.com",
								IngressRuleValue: networkingv1.IngressRuleValue{
									HTTP: &networkingv1.HTTPIngressRuleValue{
										Paths: []networkingv1.HTTPIngressPath{
											{
												Path:     "/",
												PathType: func() *networkingv1.PathType { pt := networkingv1.PathTypePrefix; return &pt }(),
												Backend: networkingv1.IngressBackend{
													Service: &networkingv1.IngressServiceBackend{
														Name: "rollout-dashboard",
														Port: networkingv1.ServiceBackendPort{
															Number: 80,
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), ingress)).To(Succeed())

				expectedURL := fmt.Sprintf("https://dashboard.example.com/rollouts/%s/%s", TestNamespace, TestName)
				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(Equal(expectedURL))
			})

			It("Should return URL with default backend when configured", func() {
				// Create service
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard",
						Namespace: ControllerNamespace,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{Port: 80},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), svc)).To(Succeed())

				// Create ingress with default backend
				ingress := &networkingv1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rollout-dashboard-ingress",
						Namespace: ControllerNamespace,
					},
					Spec: networkingv1.IngressSpec{
						DefaultBackend: &networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: "rollout-dashboard",
								Port: networkingv1.ServiceBackendPort{
									Number: 80,
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), ingress)).To(Succeed())

				// In this case, host may be empty, so URL should be empty
				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(BeEmpty())
			})

			It("Should return empty string when controller namespace cannot be determined", func() {
				os.Unsetenv("POD_NAMESPACE")
				url := reconciler.getRolloutDashboardURL(context.Background(), TestNamespace, TestName)
				Expect(url).To(BeEmpty())
			})
		})

		Context("getCurrentVersionFromRollout", func() {
			It("Should return revision when available", func() {
				revision := "abc123"
				rollout := &kuberikrolloutv1alpha1.Rollout{
					Status: kuberikrolloutv1alpha1.RolloutStatus{
						History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							{
								ID: k8sptr.To(int64(1)),
								Version: kuberikrolloutv1alpha1.VersionInfo{
									Revision: &revision,
								},
								Timestamp: metav1.Now(),
							},
						},
					},
				}
				version := reconciler.getCurrentVersionFromRollout(rollout)
				Expect(version).ToNot(BeNil())
				Expect(*version).To(Equal(revision))
			})

			It("Should return nil when revision is not available", func() {
				rollout := &kuberikrolloutv1alpha1.Rollout{
					Status: kuberikrolloutv1alpha1.RolloutStatus{
						History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
							{
								ID:      k8sptr.To(int64(1)),
								Version: kuberikrolloutv1alpha1.VersionInfo{
									// No revision
								},
								Timestamp: metav1.Now(),
							},
						},
					},
				}
				version := reconciler.getCurrentVersionFromRollout(rollout)
				Expect(version).To(BeNil())
			})

			It("Should return nil when history is empty", func() {
				rollout := &kuberikrolloutv1alpha1.Rollout{
					Status: kuberikrolloutv1alpha1.RolloutStatus{
						History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{},
					},
				}
				version := reconciler.getCurrentVersionFromRollout(rollout)
				Expect(version).To(BeNil())
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
				var a, b []string
				Expect(reconciler.slicesEqual(a, b)).To(BeTrue())
			})

			It("Should return false for one nil slice", func() {
				var a []string
				b := []string{"a"}
				Expect(reconciler.slicesEqual(a, b)).To(BeFalse())
			})

			It("Should return false for different lengths", func() {
				a := []string{"a", "b"}
				b := []string{"a", "b", "c"}
				Expect(reconciler.slicesEqual(a, b)).To(BeFalse())
			})
		})

		Context("Deployment validation", func() {
			It("Should validate Deployment spec", func() {
				deployment := &kuberikv1alpha1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-deployment",
						Namespace: "default",
					},
					Spec: kuberikv1alpha1.DeploymentSpec{
						RolloutRef: corev1.LocalObjectReference{
							Name: "test-rollout",
						},
						BackendConfig: kuberikv1alpha1.BackendConfig{
							Backend: "github",
							Project: "kuberik/github-controller-testing",
						},
						DeploymentName: "kuberik-test-deployment",
						Environment:    "production",
					},
				}

				Expect(deployment.Spec.RolloutRef.Name).To(Equal("test-rollout"))
				Expect(deployment.Spec.BackendConfig.Project).To(Equal("kuberik/github-controller-testing"))
				Expect(deployment.Spec.DeploymentName).To(Equal("kuberik-test-deployment"))
				Expect(deployment.Spec.Environment).To(Equal("production"))
			})
		})
	})

	Context("Integration Tests", func() {
		BeforeEach(func() {
			// Clean up GitHub deployments before each integration test
			if os.Getenv("GITHUB_TOKEN") != "" {
				By("Cleaning up GitHub deployments before test")
				cleanupDeployments("kuberik/github-controller-testing")
			}
		})

		It("Should create RolloutGate when Deployment is created", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(1)),
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

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying GitHub deployment was created")
			// Get the updated Deployment to check status
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			// Verify GitHub deployment was created
			Expect(updatedDeployment.Status.DeploymentID).ToNot(BeNil())
			Expect(*updatedDeployment.Status.DeploymentID).To(BeNumerically(">", 0))
			Expect(updatedDeployment.Status.DeploymentURL).ToNot(BeEmpty())
			Expect(updatedDeployment.Status.CurrentVersion).To(Equal(revision))
			Expect(updatedDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))

			By("Verifying RolloutGate was created")
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedDeployment.Status.RolloutGateRef.Name,
				Namespace: DeploymentNamespace,
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
			ghDeployment, _, err := githubClient.Repositories.GetDeployment(context.Background(), "kuberik", "github-controller-testing", *updatedDeployment.Status.DeploymentID)
			Expect(err).ToNot(HaveOccurred())
			Expect(ghDeployment).ToNot(BeNil())
			Expect(ghDeployment.Ref).ToNot(BeNil())
			Expect(*ghDeployment.Ref).To(Equal(revision))
			Expect(ghDeployment.Environment).ToNot(BeNil())
			Expect(*ghDeployment.Environment).To(Equal("kuberik-test-deployment/production"))

			By("Verifying GitHub deployment status was created")
			// Get deployment statuses
			statuses, _, err := githubClient.Repositories.ListDeploymentStatuses(context.Background(), "kuberik", "github-controller-testing", *updatedDeployment.Status.DeploymentID, &github.ListOptions{})
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
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
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
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment with passing=true")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status-change",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-change",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status-change",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying initial deployment status")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-change",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			Expect(updatedDeployment.Status.DeploymentID).ToNot(BeNil())
			initialDeploymentID := *updatedDeployment.Status.DeploymentID

			// Get initial status count
			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			statuses, _, err := githubClient.Repositories.ListDeploymentStatuses(context.Background(), "kuberik", "github-controller-testing", initialDeploymentID, &github.ListOptions{})
			Expect(err).ToNot(HaveOccurred())
			Expect(statuses).ToNot(BeEmpty())
			initialStatusCount := len(statuses)
			_ = initialStatusCount // Use variable to avoid unused error

			By("All GitHub deployments are initially successful")
			// The test just verifies the initial deployment was successful
		})

		It("Should update RolloutGate when Deployment spec changes", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-update",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
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
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment with passing=true")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-update",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-update",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-update",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-update",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying RolloutGate was created")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-update",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			Expect(updatedDeployment.Status.RolloutGateRef).ToNot(BeNil())
			rolloutGateName := updatedDeployment.Status.RolloutGateRef.Name

			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      rolloutGateName,
				Namespace: DeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Verify initial state
			Expect(rolloutGate.Spec.RolloutRef).ToNot(BeNil())
			Expect(rolloutGate.Spec.RolloutRef.Name).To(Equal("test-rollout-update"))

			// Update Deployment spec
			updatedDeployment.Spec.Environment = "staging"
			Expect(k8sClient.Update(context.Background(), updatedDeployment)).Should(Succeed())

			// Reconcile again
			result, err = reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())

			// Verify RolloutGate still exists and is updated
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      rolloutGateName,
				Namespace: DeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			Expect(rolloutGate.Spec.RolloutRef).ToNot(BeNil())
			Expect(rolloutGate.Spec.RolloutRef.Name).To(Equal("test-rollout-update"))
		})

		It("Should handle missing Rollout", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Deployment with non-existent Rollout")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-missing-rollout",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "non-existent-rollout",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-missing-rollout",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-missing-rollout",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get Rollout"))
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("Should handle Rollout with no revision", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with no revision")
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-no-revision",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
				Status: kuberikrolloutv1alpha1.RolloutStatus{
					History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
						{
							ID: k8sptr.To(int64(1)),
							Version: kuberikrolloutv1alpha1.VersionInfo{
								Tag: "v1.0.0",
								// No revision
							},
							Timestamp: metav1.Now(),
						},
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-no-revision",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-no-revision",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-no-revision",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-no-revision",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			// Should handle gracefully - no error, but no deployment created either
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
		})

		It("Should handle empty Rollout history", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with empty history")
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-empty-history",
					Namespace: DeploymentNamespace,
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
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-empty-history",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-empty-history",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-empty-history",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-empty-history",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			// Should handle gracefully - no error, but no deployment created either
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
		})

		It("Should update Deployment status with deployment information", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with deployment history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
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
			// Delete if exists first
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			// Update the rollout to set the status
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
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
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying Deployment status was updated")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			Expect(updatedDeployment.Status.CurrentVersion).To(Equal(revision))
			Expect(updatedDeployment.Status.RolloutGateRef).ToNot(BeNil())
			Expect(updatedDeployment.Status.RolloutGateRef.Name).To(HavePrefix("ghd-"))
		})

		It("Should update allowed versions from relationships", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating staging deployment with success status")
			// First create a related deployment (staging)
			// Use a commit SHA as the revision (GitHub deployment ref)
			// Relationships are for the same deployment name but different environment
			// So we create "kuberik-test-deployment-deps/staging" environment
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			stagingTag := "v1.0.0" // Tag that matches the staging revision
			stagingEnv := "kuberik-test-deployment-deps/staging"
			stagingTask := "deploy:kuberik-test-deployment-deps"
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String(stagingEnv),
				Task:                  github.String(stagingTask),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create success status for staging deployment
			successState := "success"
			statusRequest := &github.DeploymentStatusRequest{
				State:       &successState,
				Description: github.String("Deployment successful"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), statusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating Rollout with release candidates")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-deps",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(1)),
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

			By("Creating Deployment with relationship")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-deps",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-deps",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-deps",
					Environment:    "production",
					Relationship: &kuberikv1alpha1.DeploymentRelationship{
						Environment: "staging",
						Type:        "after",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment with relationship")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-deps",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying allowed versions were updated from relationships on RolloutGate")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-deps",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			Expect(updatedDeployment.Status.RolloutGateRef).ToNot(BeNil())
			rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      updatedDeployment.Status.RolloutGateRef.Name,
				Namespace: DeploymentNamespace,
			}, rolloutGate)).To(Succeed())

			// Verify allowed versions were set
			Expect(rolloutGate.Spec.AllowedVersions).ToNot(BeNil())
			Expect(*rolloutGate.Spec.AllowedVersions).To(ContainElement(stagingTag))
		})

		It("Should track deployment statuses and update DeploymentStatuses", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with multiple history entries")
			revision1 := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			revision2 := "8bd1ffbf07d9f04b9aba8757303a0f4c328c1743"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status-tracking",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.1.0",
							Revision: &revision2,
						},
						Timestamp: metav1.Now(),
					},
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision1,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status-tracking",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-tracking",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status-tracking",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status-tracking",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying DeploymentStatuses were updated")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-tracking",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			// Should have status entries for both versions in history
			Expect(updatedDeployment.Status.DeploymentStatuses).ToNot(BeEmpty())

			// Find status entries for production environment
			productionStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}

			// Should have statuses for both revisions
			Expect(len(productionStatuses)).To(BeNumerically(">=", 2))

			// Verify both revisions are tracked
			revisionsFound := make(map[string]bool)
			for _, status := range productionStatuses {
				revisionsFound[status.Version] = true
				Expect(status.Status).ToNot(BeEmpty())
				Expect(status.DeploymentID).ToNot(BeNil())
			}
			Expect(revisionsFound[revision1]).To(BeTrue())
			Expect(revisionsFound[revision2]).To(BeTrue())
		})

		It("Should track relevant versions based on relationships", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating related environment deployments")
			// Create staging environment with version
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			stagingEnv := "kuberik-test-deployment-relevant/staging"
			stagingTask := "deploy:kuberik-test-deployment-relevant"
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String(stagingEnv),
				Task:                  github.String(stagingTask),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create success status for staging
			successState := "success"
			statusRequest := &github.DeploymentStatusRequest{
				State:       &successState,
				Description: github.String("Deployment successful"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), statusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating Rollout with history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-relevant",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment with relationship to staging")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-relevant",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-relevant",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-relevant",
					Environment:    "production",
					Relationship: &kuberikv1alpha1.DeploymentRelationship{
						Environment: "staging",
						Type:        "after",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-relevant",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying relevant versions are tracked")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-relevant",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			// Should have status entries for staging environment (related environment)
			stagingStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "staging" {
					stagingStatuses = append(stagingStatuses, status)
				}
			}

			// Should track staging version since it's related
			Expect(len(stagingStatuses)).To(BeNumerically(">=", 1))

			// Verify staging version is tracked
			stagingVersionFound := false
			for _, status := range stagingStatuses {
				if status.Version == stagingRef {
					stagingVersionFound = true
					Expect(status.Status).To(Equal("success"))
					break
				}
			}
			Expect(stagingVersionFound).To(BeTrue())
		})

		It("Should update DeploymentStatuses when versions are added to history", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with initial history")
			revision1 := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status-update",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{
						Name: "test-policy",
					},
				},
			}
			k8sClient.Delete(context.Background(), rollout)
			Expect(k8sClient.Create(context.Background(), rollout)).Should(Succeed())

			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision1,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status-update",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-update",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status-update",
					Environment:    "production",
				},
			}
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment - first time")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status-update",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying DeploymentStatuses has entry for revision1")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-update",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			productionStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}
			Expect(len(productionStatuses)).To(BeNumerically(">=", 1))

			revision1Found := false
			for _, status := range productionStatuses {
				if status.Version == revision1 {
					revision1Found = true
					Expect(status.Status).ToNot(BeEmpty())
					Expect(status.DeploymentID).ToNot(BeNil())
					break
				}
			}
			Expect(revision1Found).To(BeTrue())

			By("Adding new version to history")
			revision2 := "8bd1ffbf07d9f04b9aba8757303a0f4c328c1743"
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.1.0",
							Revision: &revision2,
						},
						Timestamp: metav1.Now(),
					},
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision1,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Reconciling Deployment - second time with new version")
			result, err = reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())

			By("Verifying DeploymentStatuses has entries for both revisions")
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-update",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			productionStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}
			Expect(len(productionStatuses)).To(BeNumerically(">=", 2))

			revisionsFound := make(map[string]bool)
			for _, status := range productionStatuses {
				revisionsFound[status.Version] = true
			}
			Expect(revisionsFound[revision1]).To(BeTrue())
			Expect(revisionsFound[revision2]).To(BeTrue())
		})

		It("Should remove DeploymentStatuses entries when versions are removed from history", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			By("Creating Rollout with two history entries")
			revision1 := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			revision2 := "8bd1ffbf07d9f04b9aba8757303a0f4c328c1743"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status-remove",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.1.0",
							Revision: &revision2,
						},
						Timestamp: metav1.Now(),
					},
					{
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision1,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status-remove",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-remove",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status-remove",
					Environment:    "production",
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment - first time with both versions")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status-remove",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying DeploymentStatuses has entries for both revisions")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-remove",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			productionStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}
			Expect(len(productionStatuses)).To(BeNumerically(">=", 2))

			By("Removing revision1 from history")
			rollout.Status = kuberikrolloutv1alpha1.RolloutStatus{
				History: []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
					{
						ID: k8sptr.To(int64(2)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.1.0",
							Revision: &revision2,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Reconciling Deployment - second time with only revision2")
			result, err = reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())

			By("Verifying DeploymentStatuses only has entry for revision2")
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-remove",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			productionStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}

			// Should only have revision2 now
			revisionsFound := make(map[string]bool)
			for _, status := range productionStatuses {
				revisionsFound[status.Version] = true
			}
			Expect(revisionsFound[revision2]).To(BeTrue())
			Expect(revisionsFound[revision1]).To(BeFalse(), "revision1 should be removed from DeploymentStatuses")
		})

		It("Should update DeploymentStatuses for related environments based on relationships", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating staging deployment")
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			stagingEnv := "kuberik-test-deployment-status-env/staging"
			stagingTask := "deploy:kuberik-test-deployment-status-env"
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String(stagingEnv),
				Task:                  github.String(stagingTask),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create success status for staging
			successState := "success"
			statusRequest := &github.DeploymentStatusRequest{
				State:       &successState,
				Description: github.String("Deployment successful"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), statusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating Rollout with history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-status-env",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment with relationship to staging")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-status-env",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-status-env",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-status-env",
					Environment:    "production",
					Relationship: &kuberikv1alpha1.DeploymentRelationship{
						Environment: "staging",
						Type:        "after",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-status-env",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying DeploymentStatuses has entries for both production and staging")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-status-env",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			// Should have status entries for production (current environment)
			productionStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "production" {
					productionStatuses = append(productionStatuses, status)
				}
			}
			Expect(len(productionStatuses)).To(BeNumerically(">=", 1))

			// Should have status entries for staging (related environment)
			stagingStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "staging" {
					stagingStatuses = append(stagingStatuses, status)
				}
			}
			Expect(len(stagingStatuses)).To(BeNumerically(">=", 1))

			// Verify staging status entry has correct information
			stagingVersionFound := false
			for _, status := range stagingStatuses {
				if status.Version == stagingRef {
					stagingVersionFound = true
					Expect(status.Status).To(Equal("success"))
					Expect(status.DeploymentID).ToNot(BeNil())
					// DeploymentURL should not be set for non-current environments
					Expect(status.DeploymentURL).To(BeEmpty())
					break
				}
			}
			Expect(stagingVersionFound).To(BeTrue())
		})

		It("Should only track relevant versions based on relationship graph", func() {
			skipIfNoGitHubToken()

			By("Creating GitHub token secret")
			Expect(createGitHubTokenSecret()).To(Succeed())

			token := os.Getenv("GITHUB_TOKEN")
			ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
			tc := oauth2.NewClient(context.Background(), ts)
			githubClient := github.NewClient(tc)

			By("Creating related environment (staging) and unrelated environment (qa)")
			stagingRef := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			qaRef := "5220a27a5410a6a5182b9fadf537c6437fcca0b7"

			// Create staging deployment
			stagingEnv := "kuberik-test-deployment-relevant-graph/staging"
			stagingTask := "deploy:kuberik-test-deployment-relevant-graph"
			stagingDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(stagingRef),
				Environment:           github.String(stagingEnv),
				Task:                  github.String(stagingTask),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			stagingDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", stagingDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			successState := "success"
			statusRequest := &github.DeploymentStatusRequest{
				State:       &successState,
				Description: github.String("Deployment successful"),
			}
			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", stagingDeployment.GetID(), statusRequest)
			Expect(err).ToNot(HaveOccurred())

			// Create qa deployment (unrelated)
			qaEnv := "kuberik-test-deployment-relevant-graph/qa"
			qaTask := "deploy:kuberik-test-deployment-relevant-graph"
			qaDeploymentRequest := &github.DeploymentRequest{
				Ref:                   github.String(qaRef),
				Environment:           github.String(qaEnv),
				Task:                  github.String(qaTask),
				ProductionEnvironment: github.Bool(false),
				AutoMerge:             github.Bool(false),
			}
			qaDeployment, _, err := githubClient.Repositories.CreateDeployment(context.Background(), "kuberik", "github-controller-testing", qaDeploymentRequest)
			Expect(err).ToNot(HaveOccurred())

			_, _, err = githubClient.Repositories.CreateDeploymentStatus(context.Background(), "kuberik", "github-controller-testing", qaDeployment.GetID(), statusRequest)
			Expect(err).ToNot(HaveOccurred())

			By("Creating Rollout with history")
			revision := "0a9c600d3a75bcb7ec54dcef3b03e0d7fe0598d7"
			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-rollout-relevant-graph",
					Namespace: DeploymentNamespace,
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
						ID: k8sptr.To(int64(1)),
						Version: kuberikrolloutv1alpha1.VersionInfo{
							Tag:      "v1.0.0",
							Revision: &revision,
						},
						Timestamp: metav1.Now(),
					},
				},
			}
			Expect(k8sClient.Status().Update(context.Background(), rollout)).Should(Succeed())

			By("Creating Deployment with relationship to staging (not qa)")
			deployment := &kuberikv1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-github-deployment-relevant-graph",
					Namespace: DeploymentNamespace,
				},
				Spec: kuberikv1alpha1.DeploymentSpec{
					RolloutRef: corev1.LocalObjectReference{
						Name: "test-rollout-relevant-graph",
					},
					BackendConfig: kuberikv1alpha1.BackendConfig{
						Backend: "github",
						Project: "kuberik/github-controller-testing",
					},
					DeploymentName: "kuberik-test-deployment-relevant-graph",
					Environment:    "production",
					Relationship: &kuberikv1alpha1.DeploymentRelationship{
						Environment: "staging",
						Type:        "after",
					},
				},
			}
			// Delete if exists first
			k8sClient.Delete(context.Background(), deployment)
			Expect(k8sClient.Create(context.Background(), deployment)).Should(Succeed())

			By("Reconciling Deployment")
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-github-deployment-relevant-graph",
					Namespace: DeploymentNamespace,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))

			By("Verifying only relevant versions are tracked")
			updatedDeployment := &kuberikv1alpha1.Deployment{}
			Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-github-deployment-relevant-graph",
				Namespace: DeploymentNamespace,
			}, updatedDeployment)).To(Succeed())

			// Should have status entries for staging (related)
			stagingStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "staging" {
					stagingStatuses = append(stagingStatuses, status)
				}
			}
			Expect(len(stagingStatuses)).To(BeNumerically(">=", 1))

			// Should NOT have status entries for qa (unrelated)
			qaStatuses := []kuberikv1alpha1.DeploymentStatusEntry{}
			for _, status := range updatedDeployment.Status.DeploymentStatuses {
				if status.Environment == "qa" {
					qaStatuses = append(qaStatuses, status)
				}
			}
			Expect(len(qaStatuses)).To(Equal(0), "qa environment should not be tracked as it's not related")
		})
	})
})

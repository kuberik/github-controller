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
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kuberikv1alpha1 "github.com/kuberik/github-operator/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// GitHubDeploymentReconciler reconciles a GitHubDeployment object
type GitHubDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitHubDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the GitHubDeployment instance
	githubDeployment := &kuberikv1alpha1.GitHubDeployment{}
	err := r.Get(ctx, req.NamespacedName, githubDeployment)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to get GitHubDeployment")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Get the referenced Rollout to get the current version
	rollout, err := r.getReferencedRollout(ctx, githubDeployment)
	if err != nil {
		log.Error(err, "Failed to get referenced Rollout")
		return ctrl.Result{}, err
	}

	// Get GitHub client
	githubClient, err := r.getGitHubClient(ctx, githubDeployment)
	if err != nil {
		log.Error(err, "Failed to get GitHub client")
		return ctrl.Result{}, err
	}

	// Create or update GitHub deployment
	deploymentID, deploymentURL, err := r.createOrUpdateGitHubDeployment(ctx, githubClient, githubDeployment, rollout)
	if err != nil {
		log.Error(err, "Failed to create or update GitHub deployment")
		return ctrl.Result{}, err
	}

	// Create deployment status (always success initially)
	statusState := "success"
	statusDescription := fmt.Sprintf("GitHubDeployment %s ready", githubDeployment.Name)

	// Create deployment status
	if err := r.createDeploymentStatus(ctx, githubClient, githubDeployment, *deploymentID, statusState, statusDescription); err != nil {
		log.Error(err, "Failed to create deployment status")
		return ctrl.Result{}, err
	}

	// Update GitHubDeployment status
	if err := r.updateGitHubDeploymentStatus(ctx, githubDeployment, deploymentID, deploymentURL, rollout); err != nil {
		log.Error(err, "Failed to update GitHubDeployment status")
		return ctrl.Result{}, err
	}

	// Create or update RolloutGate
	if err := r.createOrUpdateRolloutGate(ctx, githubDeployment); err != nil {
		log.Error(err, "Failed to create or update RolloutGate")
		return ctrl.Result{}, err
	}

	// Check dependencies and update allowed versions on RolloutGate
	if err := r.updateAllowedVersionsFromDependencies(ctx, githubDeployment, githubClient); err != nil {
		log.Error(err, "Failed to update allowed versions from dependencies")
		return ctrl.Result{}, err
	}

	// Requeue every 5 minutes to keep status updated
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitHubDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikv1alpha1.GitHubDeployment{}).
		Named("githubdeployment").
		Complete(r)
}

// getReferencedRollout gets the Rollout referenced by the GitHubDeployment
func (r *GitHubDeploymentReconciler) getReferencedRollout(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment) (*kuberikrolloutv1alpha1.Rollout, error) {
	rollout := &kuberikrolloutv1alpha1.Rollout{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      githubDeployment.Spec.RolloutRef.Name,
		Namespace: githubDeployment.Namespace,
	}, rollout)
	if err != nil {
		return nil, fmt.Errorf("failed to get Rollout %s: %w", githubDeployment.Spec.RolloutRef.Name, err)
	}
	return rollout, nil
}

// getGitHubClient creates a GitHub client using the token from the specified secret
func (r *GitHubDeploymentReconciler) getGitHubClient(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment) (*github.Client, error) {
	secretName := githubDeployment.Spec.GitHubTokenSecret
	if secretName == "" {
		secretName = "github-token" // Default secret name
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: githubDeployment.Namespace,
	}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub token secret %s: %w", secretName, err)
	}

	token, exists := secret.Data["token"]
	if !exists {
		return nil, fmt.Errorf("token key not found in secret %s", secretName)
	}

	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: string(token)})
	tc := oauth2.NewClient(ctx, ts)
	return github.NewClient(tc), nil
}

// createOrUpdateGitHubDeployment creates or updates a GitHub deployment
func (r *GitHubDeploymentReconciler) createOrUpdateGitHubDeployment(ctx context.Context, client *github.Client, githubDeployment *kuberikv1alpha1.GitHubDeployment, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, error) {
	// Parse repository
	parts := strings.Split(githubDeployment.Spec.Repository, "/")
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("invalid repository format: %s", githubDeployment.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	// Get current version from rollout
	currentVersion := r.getCurrentVersionFromRollout(rollout)
	if currentVersion == nil {
		return nil, "", fmt.Errorf("no revision found in rollout history - revision field is required for GitHub deployments")
	}

	// Prepare deployment request
	deploymentRequest := &github.DeploymentRequest{
		Ref:                   currentVersion,
		Environment:           &githubDeployment.Spec.Environment,
		Description:           &githubDeployment.Spec.Description,
		AutoMerge:             &githubDeployment.Spec.AutoMerge,
		RequiredContexts:      &githubDeployment.Spec.RequiredContexts,
		ProductionEnvironment: github.Bool(githubDeployment.Spec.Environment == "production"),
	}

	// If we already have a deployment ID, check if we need to create a new deployment
	if githubDeployment.Status.GitHubDeploymentID != nil {
		existingDeployment, _, err := client.Repositories.GetDeployment(ctx, owner, repo, *githubDeployment.Status.GitHubDeploymentID)
		if err == nil {
			// Check if the version has changed
			if existingDeployment.Ref != nil && *existingDeployment.Ref == *currentVersion {
				// Version hasn't changed, return existing deployment
				return existingDeployment.ID, *existingDeployment.URL, nil
			}
			// Version has changed, we'll create a new deployment below
		}
	}

	// Create new deployment
	deployment, _, err := client.Repositories.CreateDeployment(ctx, owner, repo, deploymentRequest)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create GitHub deployment: %w", err)
	}

	return deployment.ID, *deployment.URL, nil
}

// createDeploymentStatus creates a deployment status for the given deployment
func (r *GitHubDeploymentReconciler) createDeploymentStatus(ctx context.Context, client *github.Client, githubDeployment *kuberikv1alpha1.GitHubDeployment, deploymentID int64, state string, description string) error {
	// Parse repository
	parts := strings.Split(githubDeployment.Spec.Repository, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository format: %s", githubDeployment.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	// Create deployment status request
	statusRequest := &github.DeploymentStatusRequest{
		State:       github.String(state),
		Description: github.String(description),
		Environment: &githubDeployment.Spec.Environment,
	}

	// Create the deployment status
	_, _, err := client.Repositories.CreateDeploymentStatus(ctx, owner, repo, deploymentID, statusRequest)
	if err != nil {
		return fmt.Errorf("failed to create deployment status: %w", err)
	}

	return nil
}

// getCurrentVersionFromRollout extracts the current version from the rollout's deployment history
func (r *GitHubDeploymentReconciler) getCurrentVersionFromRollout(rollout *kuberikrolloutv1alpha1.Rollout) *string {
	// Get the most recent deployment from history
	if len(rollout.Status.History) == 0 {
		return nil
	}

	// The history is ordered with the most recent deployment first
	latestDeployment := rollout.Status.History[0]

	// Always use the Revision field from VersionInfo - if not available, return nil
	if latestDeployment.Version.Revision == nil {
		return nil
	}

	return latestDeployment.Version.Revision
}

// updateGitHubDeploymentStatus updates the GitHubDeployment status with GitHub deployment information
func (r *GitHubDeploymentReconciler) updateGitHubDeploymentStatus(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment, deploymentID *int64, deploymentURL string, rollout *kuberikrolloutv1alpha1.Rollout) error {
	needsUpdate := false

	// Update GitHub deployment ID
	if githubDeployment.Status.GitHubDeploymentID == nil || *githubDeployment.Status.GitHubDeploymentID != *deploymentID {
		githubDeployment.Status.GitHubDeploymentID = deploymentID
		needsUpdate = true
	}

	// Update GitHub deployment URL
	if githubDeployment.Status.GitHubDeploymentURL != deploymentURL {
		githubDeployment.Status.GitHubDeploymentURL = deploymentURL
		needsUpdate = true
	}

	// Update last sync time
	now := metav1.Now()
	if githubDeployment.Status.LastSyncTime == nil || githubDeployment.Status.LastSyncTime.Before(&now) {
		githubDeployment.Status.LastSyncTime = &now
		needsUpdate = true
	}

	// Update current version
	currentVersion := r.getCurrentVersionFromRollout(rollout)
	if currentVersion != nil && githubDeployment.Status.CurrentVersion != *currentVersion {
		githubDeployment.Status.CurrentVersion = *currentVersion
		needsUpdate = true
	}

	if needsUpdate {
		return r.Status().Update(ctx, githubDeployment)
	}

	return nil
}

// createOrUpdateRolloutGate creates or updates the RolloutGate based on the GitHubDeployment spec
func (r *GitHubDeploymentReconciler) createOrUpdateRolloutGate(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment) error {
	// Try to get existing RolloutGate
	existingRolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      githubDeployment.Name + "-gate",
		Namespace: githubDeployment.Namespace,
	}, existingRolloutGate)

	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to get existing RolloutGate: %w", err)
		}
		// RolloutGate doesn't exist, create it
		rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      githubDeployment.Name + "-gate",
				Namespace: githubDeployment.Namespace,
			},
			Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &githubDeployment.Spec.RolloutRef,
			},
		}
		if err := r.Create(ctx, rolloutGate); err != nil {
			return fmt.Errorf("failed to create RolloutGate: %w", err)
		}
		existingRolloutGate = rolloutGate
	} else {
		// RolloutGate exists, ensure RolloutRef is set (don't overwrite other fields)
		if existingRolloutGate.Spec.RolloutRef == nil || existingRolloutGate.Spec.RolloutRef.Name != githubDeployment.Spec.RolloutRef.Name {
			existingRolloutGate.Spec.RolloutRef = &githubDeployment.Spec.RolloutRef
			if err := r.Update(ctx, existingRolloutGate); err != nil {
				return fmt.Errorf("failed to update RolloutGate: %w", err)
			}
		}
	}

	// Update the RolloutGateRef in GitHubDeployment status
	rolloutGateRef := &corev1.LocalObjectReference{Name: existingRolloutGate.Name}
	if githubDeployment.Status.RolloutGateRef == nil || githubDeployment.Status.RolloutGateRef.Name != rolloutGateRef.Name {
		githubDeployment.Status.RolloutGateRef = rolloutGateRef
		return r.Status().Update(ctx, githubDeployment)
	}

	return nil
}

// updateAllowedVersionsFromDependencies checks GitHub deployment dependencies and updates allowed versions on RolloutGate
func (r *GitHubDeploymentReconciler) updateAllowedVersionsFromDependencies(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment, client *github.Client) error {
	if len(githubDeployment.Spec.Dependencies) == 0 {
		return nil
	}

	// Parse repository
	parts := strings.Split(githubDeployment.Spec.Repository, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository format: %s", githubDeployment.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	var allowedVersions []string

	// Check each dependency
	for _, depName := range githubDeployment.Spec.Dependencies {
		// Get deployments for this dependency
		deployments, _, err := client.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
			Environment: depName,
		})
		if err != nil {
			continue
		}

		// Check each deployment for success status
		for _, deployment := range deployments {
			// Get deployment statuses
			statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, owner, repo, deployment.GetID(), nil)
			if err != nil {
				continue
			}

			// Check if any status is success
			for _, status := range statuses {
				if status.State != nil && *status.State == "success" {
					if deployment.Ref != nil {
						allowedVersions = append(allowedVersions, *deployment.Ref)
					}
					break
				}
			}
		}
	}

	// Update allowed versions on RolloutGate
	if githubDeployment.Status.RolloutGateRef == nil {
		return nil
	}

	rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      githubDeployment.Status.RolloutGateRef.Name,
		Namespace: githubDeployment.Namespace,
	}, rolloutGate)
	if err != nil {
		return fmt.Errorf("failed to get RolloutGate: %w", err)
	}

	// Check if allowedVersions need to be updated
	currentAllowedVersions := []string{}
	if rolloutGate.Spec.AllowedVersions != nil {
		currentAllowedVersions = *rolloutGate.Spec.AllowedVersions
	}

	if !r.slicesEqual(currentAllowedVersions, allowedVersions) {
		rolloutGate.Spec.AllowedVersions = &allowedVersions
		if err := r.Update(ctx, rolloutGate); err != nil {
			return fmt.Errorf("failed to update RolloutGate allowedVersions: %w", err)
		}
	}

	return nil
}

// slicesEqual compares two string slices for equality
func (r *GitHubDeploymentReconciler) slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

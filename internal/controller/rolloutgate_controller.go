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
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	"golang.org/x/oauth2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

const (
	// AnnotationKeyGateClass is the annotation key for gate class
	AnnotationKeyGateClass = "kuberik.com/gate-class"
	// AnnotationValueGitHubGateClass is the value for GitHub gate class
	AnnotationValueGitHubGateClass = "github"
	// AnnotationKeyGitHubRepo is the annotation key for GitHub repository
	AnnotationKeyGitHubRepo = "kuberik.com/github-repo"
	// AnnotationKeyGitHubToken is the annotation key for GitHub token secret reference
	AnnotationKeyGitHubToken = "kuberik.com/github-token"
	// AnnotationKeyDeploymentName is the annotation key for deployment name
	AnnotationKeyDeploymentName = "kuberik.com/deployment-name"
	// AnnotationKeyDependencies is the annotation key for dependencies
	AnnotationKeyDependencies = "kuberik.com/dependencies"
	// AnnotationKeyEnvironment is the annotation key for environment
	AnnotationKeyEnvironment = "kuberik.com/environment"
	// AnnotationKeyRef is the annotation key for Git reference
	AnnotationKeyRef = "kuberik.com/ref"
	// AnnotationKeyDescription is the annotation key for description
	AnnotationKeyDescription = "kuberik.com/description"
	// AnnotationKeyAutoMerge is the annotation key for auto merge
	AnnotationKeyAutoMerge = "kuberik.com/auto-merge"
	// AnnotationKeyRequiredContexts is the annotation key for required contexts
	AnnotationKeyRequiredContexts = "kuberik.com/required-contexts"

	// ConditionTypeGitHubDeployment is the condition type for GitHub deployment
	ConditionTypeGitHubDeployment = "GitHubDeployment"
	// ConditionTypeDependencies is the condition type for dependencies
	ConditionTypeDependencies = "Dependencies"
)

// GitHubConfig represents GitHub configuration extracted from annotations
type GitHubConfig struct {
	Repository       string
	DeploymentName   string
	Dependencies     []string
	Environment      string
	Ref              string
	Description      string
	AutoMerge        bool
	RequiredContexts []string
}

// RolloutGateReconciler reconciles a RolloutGate object
type RolloutGateReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts/status,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *RolloutGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the RolloutGate instance
	rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
	err := r.Get(ctx, req.NamespacedName, rolloutGate)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Return and don't requeue
			log.Info("RolloutGate resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get RolloutGate")
		return ctrl.Result{}, err
	}

	// Check if this RolloutGate should be managed by our controller
	if !r.isGitHubGate(rolloutGate) {
		log.Info("RolloutGate is not a GitHub gate, skipping")
		return ctrl.Result{}, nil
	}

	// Get GitHub configuration from annotations
	githubConfig, err := r.getGitHubConfig(ctx, rolloutGate)
	if err != nil {
		log.Error(err, "Failed to get GitHub configuration")
		return ctrl.Result{}, err
	}

	// Get GitHub client
	githubClient, err := r.getGitHubClient(ctx, rolloutGate)
	if err != nil {
		log.Error(err, "Failed to get GitHub client")
		return ctrl.Result{}, err
	}

	// Get the referenced Rollout to get the current version
	rollout, err := r.getReferencedRollout(ctx, rolloutGate)
	if err != nil {
		log.Error(err, "Failed to get referenced Rollout")
		return ctrl.Result{}, err
	}

	// Create or update GitHub deployment
	deploymentID, deploymentURL, err := r.createOrUpdateGitHubDeployment(ctx, githubClient, githubConfig, rolloutGate, rollout)
	if err != nil {
		log.Error(err, "Failed to create or update GitHub deployment")
		return ctrl.Result{}, err
	}

	// Update RolloutGate status
	if err := r.updateRolloutGateStatus(ctx, rolloutGate, deploymentID, deploymentURL); err != nil {
		log.Error(err, "Failed to update RolloutGate status")
		return ctrl.Result{}, err
	}

	// Check dependencies and update allowed versions
	if err := r.updateAllowedVersionsFromDependencies(ctx, rolloutGate, githubClient, githubConfig); err != nil {
		log.Error(err, "Failed to update allowed versions from dependencies")
		return ctrl.Result{}, err
	}

	// Requeue every 5 minutes to keep status updated
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// isGitHubGate checks if the RolloutGate is a GitHub gate
func (r *RolloutGateReconciler) isGitHubGate(rolloutGate *kuberikrolloutv1alpha1.RolloutGate) bool {
	// Check annotation for gate class
	if rolloutGate.Annotations != nil {
		if gateClass, exists := rolloutGate.Annotations[AnnotationKeyGateClass]; exists {
			return gateClass == AnnotationValueGitHubGateClass
		}
	}
	return false
}

// getGitHubConfig extracts GitHub configuration from annotations
func (r *RolloutGateReconciler) getGitHubConfig(ctx context.Context, rolloutGate *kuberikrolloutv1alpha1.RolloutGate) (*GitHubConfig, error) {
	if rolloutGate.Annotations == nil {
		return nil, fmt.Errorf("no GitHub configuration found in annotations")
	}

	config := &GitHubConfig{}

	// Required fields
	repo, exists := rolloutGate.Annotations[AnnotationKeyGitHubRepo]
	if !exists {
		return nil, fmt.Errorf("missing required annotation %s", AnnotationKeyGitHubRepo)
	}
	config.Repository = repo

	deploymentName, exists := rolloutGate.Annotations[AnnotationKeyDeploymentName]
	if !exists {
		return nil, fmt.Errorf("missing required annotation %s", AnnotationKeyDeploymentName)
	}
	config.DeploymentName = deploymentName

	// Optional fields
	if env, exists := rolloutGate.Annotations[AnnotationKeyEnvironment]; exists {
		config.Environment = env
	}
	if ref, exists := rolloutGate.Annotations[AnnotationKeyRef]; exists {
		config.Ref = ref
	}
	if desc, exists := rolloutGate.Annotations[AnnotationKeyDescription]; exists {
		config.Description = desc
	}
	if autoMerge, exists := rolloutGate.Annotations[AnnotationKeyAutoMerge]; exists {
		config.AutoMerge = autoMerge == "true"
	}
	if deps, exists := rolloutGate.Annotations[AnnotationKeyDependencies]; exists {
		config.Dependencies = strings.Split(deps, ",")
		for i, dep := range config.Dependencies {
			config.Dependencies[i] = strings.TrimSpace(dep)
		}
	}
	if contexts, exists := rolloutGate.Annotations[AnnotationKeyRequiredContexts]; exists {
		config.RequiredContexts = strings.Split(contexts, ",")
		for i, ctx := range config.RequiredContexts {
			config.RequiredContexts[i] = strings.TrimSpace(ctx)
		}
	}

	return config, nil
}

// getGitHubClient creates a GitHub client using the token from secret
func (r *RolloutGateReconciler) getGitHubClient(ctx context.Context, rolloutGate *kuberikrolloutv1alpha1.RolloutGate) (*github.Client, error) {
	// Get token from secret
	tokenSecretName := "github-token" // default
	if rolloutGate.Annotations != nil {
		if secretName, exists := rolloutGate.Annotations[AnnotationKeyGitHubToken]; exists {
			tokenSecretName = secretName
		}
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: rolloutGate.Namespace,
		Name:      tokenSecretName,
	}, secret)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub token secret: %w", err)
	}

	token, exists := secret.Data["token"]
	if !exists {
		return nil, fmt.Errorf("token not found in secret %s", tokenSecretName)
	}

	// Create GitHub client
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: string(token)},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	return client, nil
}

// getReferencedRollout gets the Rollout referenced by the RolloutGate
func (r *RolloutGateReconciler) getReferencedRollout(ctx context.Context, rolloutGate *kuberikrolloutv1alpha1.RolloutGate) (*kuberikrolloutv1alpha1.Rollout, error) {
	if rolloutGate.Spec.RolloutRef == nil {
		return nil, fmt.Errorf("no rollout reference specified")
	}

	rollout := &kuberikrolloutv1alpha1.Rollout{}
	err := r.Get(ctx, types.NamespacedName{
		Namespace: rolloutGate.Namespace,
		Name:      rolloutGate.Spec.RolloutRef.Name,
	}, rollout)
	if err != nil {
		return nil, fmt.Errorf("failed to get referenced Rollout: %w", err)
	}

	return rollout, nil
}

// createOrUpdateGitHubDeployment creates or updates a GitHub deployment
func (r *RolloutGateReconciler) createOrUpdateGitHubDeployment(ctx context.Context, client *github.Client, config *GitHubConfig, rolloutGate *kuberikrolloutv1alpha1.RolloutGate, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, error) {
	// Parse repository
	parts := strings.Split(config.Repository, "/")
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("invalid repository format: %s", config.Repository)
	}
	owner, repo := parts[0], parts[1]

	// Get the current version from rollout history
	currentVersion := r.getCurrentVersionFromRollout(rollout)
	if currentVersion == nil {
		return nil, "", fmt.Errorf("no revision found in rollout history - revision field is required for GitHub deployments")
	}

	// Prepare deployment request
	deploymentRequest := &github.DeploymentRequest{
		Ref:                   currentVersion, // Use the current version from rollout
		Environment:           &config.Environment,
		Description:           &config.Description,
		AutoMerge:             &config.AutoMerge,
		RequiredContexts:      &config.RequiredContexts,
		ProductionEnvironment: github.Bool(config.Environment == "production"),
	}

	// If we already have a deployment ID, try to update it
	if rolloutGate.Annotations != nil {
		if deploymentIDStr, exists := rolloutGate.Annotations["kuberik.com/github-deployment-id"]; exists {
			if deploymentID, err := strconv.ParseInt(deploymentIDStr, 10, 64); err == nil {
				deployment, _, err := client.Repositories.GetDeployment(ctx, owner, repo, deploymentID)
				if err == nil {
					// Update existing deployment
					deployment, _, err = client.Repositories.CreateDeployment(ctx, owner, repo, deploymentRequest)
					if err != nil {
						return nil, "", fmt.Errorf("failed to update GitHub deployment: %w", err)
					}
					return deployment.ID, *deployment.URL, nil
				}
			}
		}
	}

	// Create new deployment
	deployment, _, err := client.Repositories.CreateDeployment(ctx, owner, repo, deploymentRequest)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create GitHub deployment: %w", err)
	}

	return deployment.ID, *deployment.URL, nil
}

// getCurrentVersionFromRollout extracts the current version from the rollout's deployment history
func (r *RolloutGateReconciler) getCurrentVersionFromRollout(rollout *kuberikrolloutv1alpha1.Rollout) *string {
	// Get the most recent deployment from history
	if len(rollout.Status.History) == 0 {
		return nil
	}

	return rollout.Status.History[0].Version.Revision
}

// updateRolloutGateStatus updates the RolloutGate status with GitHub deployment information
func (r *RolloutGateReconciler) updateRolloutGateStatus(ctx context.Context, rolloutGate *kuberikrolloutv1alpha1.RolloutGate, deploymentID *int64, deploymentURL string) error {
	// Store GitHub deployment information in annotations since the status field is empty
	needsUpdate := false

	if rolloutGate.Annotations == nil {
		rolloutGate.Annotations = make(map[string]string)
	}

	// Update GitHub deployment ID annotation
	if deploymentID != nil {
		newID := fmt.Sprintf("%d", *deploymentID)
		if rolloutGate.Annotations["kuberik.com/github-deployment-id"] != newID {
			rolloutGate.Annotations["kuberik.com/github-deployment-id"] = newID
			needsUpdate = true
		}
	}

	// Update GitHub deployment URL annotation
	if rolloutGate.Annotations["kuberik.com/github-deployment-url"] != deploymentURL {
		rolloutGate.Annotations["kuberik.com/github-deployment-url"] = deploymentURL
		needsUpdate = true
	}

	// Update last sync time annotation
	now := time.Now().Format(time.RFC3339)
	if rolloutGate.Annotations["kuberik.com/github-last-sync-time"] != now {
		rolloutGate.Annotations["kuberik.com/github-last-sync-time"] = now
		needsUpdate = true
	}

	if needsUpdate {
		return r.Update(ctx, rolloutGate)
	}

	return nil
}

// updateAllowedVersionsFromDependencies updates allowed versions based on dependency deployments
func (r *RolloutGateReconciler) updateAllowedVersionsFromDependencies(ctx context.Context, rolloutGate *kuberikrolloutv1alpha1.RolloutGate, client *github.Client, config *GitHubConfig) error {
	if len(config.Dependencies) == 0 {
		return nil
	}

	// Parse repository
	parts := strings.Split(config.Repository, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository format: %s", config.Repository)
	}
	owner, repo := parts[0], parts[1]

	var allowedVersions []string

	// Check each dependency deployment
	for _, depName := range config.Dependencies {
		// Get deployments for this dependency
		deployments, _, err := client.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
			Environment: depName,
		})
		if err != nil {
			return fmt.Errorf("failed to list deployments for dependency %s: %w", depName, err)
		}

		// Find successful deployments
		for _, deployment := range deployments {
			// Get deployment statuses
			statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, owner, repo, deployment.GetID(), nil)
			if err != nil {
				continue
			}

			// Check if any status is success
			for _, status := range statuses {
				if status.GetState() == "success" {
					// Use the deployment ref as the version
					version := deployment.GetRef()
					if version != "" {
						allowedVersions = append(allowedVersions, version)
					}
					break
				}
			}
		}
	}

	// Update allowed versions if they have changed
	if !r.slicesEqual(rolloutGate.Spec.AllowedVersions, &allowedVersions) {
		rolloutGate.Spec.AllowedVersions = &allowedVersions
		if err := r.Update(ctx, rolloutGate); err != nil {
			return fmt.Errorf("failed to update allowed versions: %w", err)
		}
	}

	return nil
}

// slicesEqual checks if two string slices are equal
func (r *RolloutGateReconciler) slicesEqual(a, b *[]string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(*a) != len(*b) {
		return false
	}
	for i := range *a {
		if (*a)[i] != (*b)[i] {
			return false
		}
	}
	return true
}

// enqueueRolloutGatesForRollout enqueues RolloutGate objects that reference a given Rollout
func (r *RolloutGateReconciler) enqueueRolloutGatesForRollout(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout, ok := obj.(*kuberikrolloutv1alpha1.Rollout)
	if !ok {
		return []reconcile.Request{}
	}

	// Find all RolloutGate resources that might reference this Rollout
	rolloutGates := &kuberikrolloutv1alpha1.RolloutGateList{}
	err := r.List(ctx, rolloutGates, client.InNamespace(rollout.Namespace))
	if err != nil {
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, rolloutGate := range rolloutGates.Items {
		// Check if this RolloutGate references our Rollout
		if rolloutGate.Spec.RolloutRef != nil && rolloutGate.Spec.RolloutRef.Name == rollout.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      rolloutGate.Name,
					Namespace: rolloutGate.Namespace,
				},
			})
		}
	}

	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *RolloutGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikrolloutv1alpha1.RolloutGate{}).
		Watches(
			&kuberikrolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueRolloutGatesForRollout),
		).
		Named("rolloutgate").
		Complete(r)
}

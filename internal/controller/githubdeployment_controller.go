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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bartventer/httpcache"
	_ "github.com/bartventer/httpcache/store/memcache" // Register the in-memory backend
	"github.com/google/go-github/v62/github"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikv1alpha1 "github.com/kuberik/github-operator/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// deploymentPayload represents the payload stored in GitHub deployments for mapping
type deploymentPayload struct {
	ID string `json:"id"` // Stored as string in JSON for consistency
}

// deploymentKey represents a unique key for mapping deployments by ID and environment
type deploymentKey struct {
	ID          string
	Environment string
}

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

	// Sync entire rollout history with GitHub deployments and statuses
	deploymentID, deploymentURL, err := r.syncDeploymentHistory(ctx, githubClient, githubDeployment, rollout)
	if err != nil {
		log.Error(err, "Failed to sync deployment history")
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

	// Requeue at configured interval (default 1 minute) to keep status updated
	requeueInterval := time.Minute // default
	if githubDeployment.Spec.RequeueInterval != "" {
		parsed, err := time.ParseDuration(githubDeployment.Spec.RequeueInterval)
		if err != nil {
			log.Error(err, "Invalid RequeueInterval, using default", "interval", githubDeployment.Spec.RequeueInterval)
		} else {
			requeueInterval = parsed
		}
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitHubDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikv1alpha1.GitHubDeployment{}).
		Watches(
			&kuberikrolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.rolloutToGitHubDeployment),
		).
		Named("githubdeployment").
		Complete(r)
}

// rolloutToGitHubDeployment maps a Rollout to all GitHubDeployments that reference it
func (r *GitHubDeploymentReconciler) rolloutToGitHubDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout := obj.(*kuberikrolloutv1alpha1.Rollout)
	requests := []reconcile.Request{}

	// List all GitHubDeployments in the same namespace
	githubDeploymentList := &kuberikv1alpha1.GitHubDeploymentList{}
	if err := r.List(ctx, githubDeploymentList, client.InNamespace(rollout.Namespace)); err != nil {
		return requests
	}

	// Find all GitHubDeployments that reference this Rollout
	for i := range githubDeploymentList.Items {
		githubDeployment := &githubDeploymentList.Items[i]
		if githubDeployment.Spec.RolloutRef.Name == rollout.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      githubDeployment.Name,
					Namespace: githubDeployment.Namespace,
				},
			})
		}
	}

	return requests
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
// The client uses conditional requests with caching to reduce API rate limit consumption
// Each token gets its own client with its own cache to ensure proper isolation
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

	// Create HTTP client with memory cache for conditional requests
	// Each client gets its own cache, ensuring different tokens have isolated caches
	httpClient := httpcache.NewClient("memcache://")

	// Create GitHub client with OAuth2 authentication and caching
	return github.NewClient(httpClient).WithAuthToken(string(token)), nil
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

// extractDeploymentKey extracts the deployment key from a GitHub deployment's payload and environment
func (r *GitHubDeploymentReconciler) extractDeploymentKey(dep *github.Deployment) *deploymentKey {
	if len(dep.Payload) == 0 || dep.Environment == nil {
		return nil
	}

	// Try to decode payload - GitHub API may return it as base64-encoded string or JSON object
	var payloadBytes []byte

	// First try as base64-encoded string
	var payloadStr string
	if err := json.Unmarshal(dep.Payload, &payloadStr); err == nil {
		// Decode base64
		if decoded, err := base64.StdEncoding.DecodeString(payloadStr); err == nil {
			payloadBytes = decoded
		} else {
			// Not base64, use string as-is
			payloadBytes = []byte(payloadStr)
		}
	} else {
		// Not a string, use payload directly
		payloadBytes = dep.Payload
	}

	// Skip empty objects
	if string(payloadBytes) == "{}" || string(payloadBytes) == "null" {
		return nil
	}

	var payload deploymentPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil || payload.ID == "" {
		return nil
	}

	return &deploymentKey{
		ID:          payload.ID,
		Environment: *dep.Environment,
	}
}

// syncDeploymentHistory ensures a GitHub deployment exists for each rollout history entry
// and posts a DeploymentStatus matching each entry's bake status. It returns the latest
// deployment's ID and URL for status bookkeeping on the CR.
func (r *GitHubDeploymentReconciler) syncDeploymentHistory(ctx context.Context, gh *github.Client, ghd *kuberikv1alpha1.GitHubDeployment, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, error) {
	// Parse repository
	parts := strings.Split(ghd.Spec.Repository, "/")
	if len(parts) != 2 {
		return nil, "", fmt.Errorf("invalid repository format: %s", ghd.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	// Map deployments by ID + environment from payload
	// We'll query deployments per history entry using ref + environment for more targeted queries
	keyToDeployment := make(map[deploymentKey]*github.Deployment)

	// Iterate history from oldest to newest so states evolve in order
	for i := len(rollout.Status.History) - 1; i >= 0; i-- {
		h := rollout.Status.History[i]
		if h.Version.Revision == nil || *h.Version.Revision == "" {
			continue
		}
		ref := *h.Version.Revision

		// Skip entries without an ID as we need it for unique mapping
		if h.ID == nil {
			continue
		}
		historyID := fmt.Sprintf("%d", *h.ID)

		// Create key for this history entry using ID + environment
		key := deploymentKey{
			ID:          historyID,
			Environment: ghd.Spec.Environment,
		}

		// Check if we already found this deployment in a previous iteration
		dep := keyToDeployment[key]

		// If not found, query deployments for this specific ref + environment
		if dep == nil {
			deployments, _, err := gh.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
				Ref:         ref,
				Environment: ghd.Spec.Environment,
			})
			if err != nil {
				return nil, "", fmt.Errorf("failed to list deployments for ref %s: %w", ref, err)
			}

			// Check all deployments to find one with our ID
			for _, d := range deployments {
				if d.ID == nil {
					continue
				}

				// Try to extract key from list response first (faster)
				existingKey := r.extractDeploymentKey(d)
				if existingKey != nil && existingKey.ID == historyID {
					dep = d
					keyToDeployment[key] = dep
					break
				}

				// If payload extraction failed, fetch individually to get full payload
				fullDeployment, _, err := gh.Repositories.GetDeployment(ctx, owner, repo, *d.ID)
				if err != nil {
					continue
				}
				existingKey = r.extractDeploymentKey(fullDeployment)
				if existingKey != nil && existingKey.ID == historyID {
					dep = fullDeployment
					keyToDeployment[key] = dep
					break
				}
			}
		}

		if dep == nil {
			// Create missing deployment for this history entry
			payload := deploymentPayload{ID: historyID}
			payloadJSON, err := json.Marshal(payload)
			if err != nil {
				return nil, "", fmt.Errorf("failed to marshal deployment payload: %w", err)
			}

			req := &github.DeploymentRequest{
				Ref:                   &ref,
				Environment:           &ghd.Spec.Environment,
				Description:           h.Message,
				RequiredContexts:      &ghd.Spec.RequiredContexts,
				ProductionEnvironment: github.Bool(ghd.Spec.Environment == "production"),
				AutoMerge:             github.Bool(false),
				Payload:               payloadJSON,
			}
			created, _, err := gh.Repositories.CreateDeployment(ctx, owner, repo, req)
			if err != nil {
				return nil, "", fmt.Errorf("failed to create GitHub deployment: %w", err)
			}
			dep = created
			keyToDeployment[key] = dep
		}

		// Determine desired GH status for this history entry
		ghState, ghDesc := mapBakeToGitHubState(h.BakeStatus)

		// Get latest status for this deployment to avoid duplicate posts
		statuses, _, err := gh.Repositories.ListDeploymentStatuses(ctx, owner, repo, dep.GetID(), &github.ListOptions{PerPage: 1})
		if err == nil && len(statuses) > 0 {
			if statuses[0].State != nil && *statuses[0].State == ghState && statuses[0].Description != nil && *statuses[0].Description == ghDesc {
				// Up-to-date, skip
			} else {
				if err := r.createDeploymentStatus(ctx, gh, ghd, dep.GetID(), ghState, ghDesc); err != nil {
					return nil, "", err
				}
			}
		} else {
			if err := r.createDeploymentStatus(ctx, gh, ghd, dep.GetID(), ghState, ghDesc); err != nil {
				return nil, "", err
			}
		}
	}

	// Find the latest entry with valid ID and revision (skip entries without ID)
	var latest *kuberikrolloutv1alpha1.DeploymentHistoryEntry
	for i := 0; i < len(rollout.Status.History); i++ {
		h := rollout.Status.History[i]
		if h.ID != nil && h.Version.Revision != nil && *h.Version.Revision != "" {
			// Copy the entry to avoid pointer issues
			entry := h
			latest = &entry
			break
		}
	}

	if latest == nil {
		return nil, "", fmt.Errorf("no valid history entry found with both ID and revision")
	}

	latestID := fmt.Sprintf("%d", *latest.ID)
	latestKey := deploymentKey{
		ID:          latestID,
		Environment: ghd.Spec.Environment,
	}

	if dep := keyToDeployment[latestKey]; dep != nil {
		return dep.ID, dep.GetURL(), nil
	}

	// This shouldn't happen if we processed history correctly, but handle it
	return nil, "", fmt.Errorf("failed to find or create deployment for ID %s and environment %s", latestID, ghd.Spec.Environment)
}

// mapBakeToGitHubState maps bake status/message to GitHub DeploymentStatus state and description
func mapBakeToGitHubState(bakeStatus *string) (string, string) {
	var status string
	if bakeStatus != nil {
		status = *bakeStatus
	}
	switch status {
	case kuberikrolloutv1alpha1.BakeStatusSucceeded:
		return "success", "Bake succeeded"
	case kuberikrolloutv1alpha1.BakeStatusFailed:
		return "failure", "Bake failed"
	case kuberikrolloutv1alpha1.BakeStatusInProgress:
		return "in_progress", "Baking in progress"
	case kuberikrolloutv1alpha1.BakeStatusCancelled:
		return "inactive", "Bake cancelled"
	default:
		return "pending", "Deployment in progress"
	}
}

// createOrUpdateRolloutGate creates or updates the RolloutGate based on the GitHubDeployment spec
func (r *GitHubDeploymentReconciler) createOrUpdateRolloutGate(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment) error {
	// List all RolloutGates in the namespace to find one owned by this GitHubDeployment
	rolloutGateList := &kuberikrolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, rolloutGateList, client.InNamespace(githubDeployment.Namespace)); err != nil {
		return fmt.Errorf("failed to list RolloutGates: %w", err)
	}

	// Find existing RolloutGate owned by this GitHubDeployment
	var existingRolloutGate *kuberikrolloutv1alpha1.RolloutGate
	for i := range rolloutGateList.Items {
		gate := &rolloutGateList.Items[i]
		for _, ownerRef := range gate.OwnerReferences {
			if ownerRef.Kind == "GitHubDeployment" &&
				ownerRef.APIVersion == kuberikv1alpha1.GroupVersion.String() &&
				ownerRef.Name == githubDeployment.Name {
				// If UID is set, it must match; otherwise match by name and kind only
				if ownerRef.UID == "" || ownerRef.UID == githubDeployment.UID {
					existingRolloutGate = gate
					break
				}
			}
		}
		if existingRolloutGate != nil {
			break
		}
	}

	if existingRolloutGate == nil {
		// RolloutGate doesn't exist, create it
		rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "ghd-",
				Namespace:    githubDeployment.Namespace,
			},
			Spec: kuberikrolloutv1alpha1.RolloutGateSpec{
				RolloutRef: &githubDeployment.Spec.RolloutRef,
			},
		}

		// Set owner reference
		if err := ctrl.SetControllerReference(githubDeployment, rolloutGate, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference: %w", err)
		}

		if err := r.Create(ctx, rolloutGate); err != nil {
			return fmt.Errorf("failed to create RolloutGate: %w", err)
		}
		existingRolloutGate = rolloutGate
	} else {
		// RolloutGate exists, ensure owner reference and RolloutRef are up to date
		needsUpdate := false

		// Check and update owner reference if needed
		ownerRefFound := false
		for i := range existingRolloutGate.OwnerReferences {
			if existingRolloutGate.OwnerReferences[i].Kind == "GitHubDeployment" &&
				existingRolloutGate.OwnerReferences[i].APIVersion == kuberikv1alpha1.GroupVersion.String() &&
				existingRolloutGate.OwnerReferences[i].Name == githubDeployment.Name {
				ownerRefFound = true
				if existingRolloutGate.OwnerReferences[i].UID != githubDeployment.UID {
					// Update owner reference UID if it doesn't match
					existingRolloutGate.OwnerReferences[i].UID = githubDeployment.UID
					needsUpdate = true
				}
				break
			}
		}
		if !ownerRefFound {
			// Owner reference missing, set it
			if err := ctrl.SetControllerReference(githubDeployment, existingRolloutGate, r.Scheme); err != nil {
				return fmt.Errorf("failed to set owner reference: %w", err)
			}
			needsUpdate = true
		}

		// Ensure RolloutRef is set (don't overwrite other fields)
		if existingRolloutGate.Spec.RolloutRef == nil || existingRolloutGate.Spec.RolloutRef.Name != githubDeployment.Spec.RolloutRef.Name {
			existingRolloutGate.Spec.RolloutRef = &githubDeployment.Spec.RolloutRef
			needsUpdate = true
		}

		if needsUpdate {
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
// It matches GitHub deployment revisions to tags in the Rollout's releaseCandidates and sets those tags in AllowedVersions
func (r *GitHubDeploymentReconciler) updateAllowedVersionsFromDependencies(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment, client *github.Client) error {
	if len(githubDeployment.Spec.Dependencies) == 0 {
		return nil
	}

	// Get the referenced Rollout to access releaseCandidates
	rollout, err := r.getReferencedRollout(ctx, githubDeployment)
	if err != nil {
		return fmt.Errorf("failed to get referenced Rollout: %w", err)
	}

	// Build a map of revision -> tag from releaseCandidates
	revisionToTag := make(map[string]string)
	for _, candidate := range rollout.Status.ReleaseCandidates {
		if candidate.Revision != nil && *candidate.Revision != "" && candidate.Tag != "" {
			revisionToTag[*candidate.Revision] = candidate.Tag
		}
	}

	// Parse repository
	parts := strings.Split(githubDeployment.Spec.Repository, "/")
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository format: %s", githubDeployment.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	var allowedTags []string
	seenTags := make(map[string]bool) // Avoid duplicates

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
						// Match revision to tag in releaseCandidates
						if tag, found := revisionToTag[*deployment.Ref]; found {
							// Only add if not already seen
							if !seenTags[tag] {
								allowedTags = append(allowedTags, tag)
								seenTags[tag] = true
							}
						}
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
	err = r.Get(ctx, types.NamespacedName{
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

	if !r.slicesEqual(currentAllowedVersions, allowedTags) {
		rolloutGate.Spec.AllowedVersions = &allowedTags
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

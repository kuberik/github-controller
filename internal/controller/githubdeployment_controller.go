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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/v62/github"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kuberikv1alpha1 "github.com/kuberik/github-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// deploymentPayload represents the payload stored in GitHub deployments for mapping
type deploymentPayload struct {
	ID           string   `json:"id"`                     // Stored as string in JSON for consistency
	Dependencies []string `json:"dependencies,omitempty"` // Dependencies for this environment
}

// deploymentKey represents a unique key for mapping deployments by ID and environment
type deploymentKey struct {
	ID          string
	Environment string
}

// versionDeploymentInfo holds deployment information for a version
type versionDeploymentInfo struct {
	Status        string
	DeploymentID  *int64
	DeploymentURL string
}

// GitHubDeploymentReconciler reconciles a GitHubDeployment object
type GitHubDeploymentReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	CacheTransport http.RoundTripper
}

// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=github.kuberik.com,resources=githubdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
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
	deploymentID, deploymentURL, currentEnvDeployments, err := r.syncDeploymentHistory(ctx, githubClient, githubDeployment, rollout)
	if err != nil {
		log.Error(err, "Failed to sync deployment history")
		return ctrl.Result{}, err
	}

	// Update GitHubDeployment status
	if err := r.updateGitHubDeploymentStatus(ctx, githubDeployment, deploymentID, deploymentURL, rollout, currentEnvDeployments); err != nil {
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
// The client uses ghcache for conditional requests with caching to reduce API rate limit consumption
// ghcache automatically partitions the cache by auth header, ensuring proper token isolation
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

	// Create HTTP client with ghcache transport
	// If CacheTransport is not set, use default transport (for tests)
	transport := r.CacheTransport
	if transport == nil {
		transport = http.DefaultTransport
	}

	httpClient := &http.Client{
		Transport: transport,
	}

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

	// Format environment as "deploymentName:environment" for GitHub
	formattedEnv := formatDeploymentEnvironment(githubDeployment.Spec.DeploymentName, githubDeployment.Spec.Environment)

	// Get rollout-dashboard URL from ingress/gateway in controller's namespace
	// The URL will include the path /rollouts/<namespace>/<name>
	environmentURL := r.getRolloutDashboardURL(ctx, githubDeployment.Namespace, githubDeployment.Name)

	// Create deployment status request
	statusRequest := &github.DeploymentStatusRequest{
		State:       github.String(state),
		Description: github.String(description),
		Environment: &formattedEnv,
	}
	if environmentURL != "" {
		statusRequest.EnvironmentURL = github.String(environmentURL)
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
func (r *GitHubDeploymentReconciler) updateGitHubDeploymentStatus(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment, deploymentID *int64, deploymentURL string, rollout *kuberikrolloutv1alpha1.Rollout, currentEnvDeployments map[string]versionDeploymentInfo) error {
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

	// Update deployment statuses for current environment
	// Only keep versions that are still in history
	if githubDeployment.Status.DeploymentStatuses == nil {
		githubDeployment.Status.DeploymentStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
	}

	currentEnv := githubDeployment.Spec.Environment

	// Build set of versions that should be kept (only those in history)
	versionsInHistory := make(map[string]bool)
	for _, h := range rollout.Status.History {
		if h.Version.Revision != nil && *h.Version.Revision != "" {
			versionsInHistory[*h.Version.Revision] = true
		}
	}

	// Remove all entries for current environment that are no longer in history
	statuses := githubDeployment.Status.DeploymentStatuses
	statuses = removeDeploymentStatusEntries(statuses, func(entry kuberikv1alpha1.DeploymentStatusEntry) bool {
		return entry.Environment == currentEnv && !versionsInHistory[entry.Version]
	})

	// Update or add statuses for versions in history
	if currentEnvDeployments != nil {
		for version, depInfo := range currentEnvDeployments {
			if versionsInHistory[version] {
				statuses = updateDeploymentStatusEntryWithInfo(statuses, currentEnv, version, depInfo.Status, depInfo.DeploymentID, depInfo.DeploymentURL)
			}
		}
	}

	// Check if update is needed
	if !deploymentStatusesEqual(githubDeployment.Status.DeploymentStatuses, statuses) {
		githubDeployment.Status.DeploymentStatuses = statuses
		needsUpdate = true
	}

	if needsUpdate {
		return r.Status().Update(ctx, githubDeployment)
	}

	return nil
}

// formatDeploymentTask formats the deployment name into the task format "deploy:<name>"
func formatDeploymentTask(deploymentName string) string {
	return fmt.Sprintf("deploy:%s", deploymentName)
}

// formatDeploymentEnvironment formats the environment as "deploymentName:environment"
func formatDeploymentEnvironment(deploymentName, environment string) string {
	return fmt.Sprintf("%s/%s", deploymentName, environment)
}

// getControllerNamespace gets the namespace where the controller is running.
// It reads from the service account namespace file, or falls back to environment variable.
func (r *GitHubDeploymentReconciler) getControllerNamespace() string {
	// Try reading from service account namespace file (standard in Kubernetes pods)
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	// Fallback to environment variable
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	// Last resort: try WATCH_NAMESPACE (used by some operators)
	if ns := os.Getenv("WATCH_NAMESPACE"); ns != "" {
		return ns
	}
	return ""
}

// getRolloutDashboardURL finds the URL for rollout-dashboard service from ingress or gateway
// in the controller's namespace. Returns empty string if not found.
// The URL will have the path /rollouts/<githubDeploymentNamespace>/<githubDeploymentName> appended.
func (r *GitHubDeploymentReconciler) getRolloutDashboardURL(ctx context.Context, githubDeploymentNamespace, githubDeploymentName string) string {
	// Get the controller's namespace
	controllerNamespace := r.getControllerNamespace()
	if controllerNamespace == "" {
		return ""
	}

	// First, check if rollout-dashboard service exists in controller's namespace
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "rollout-dashboard",
		Namespace: controllerNamespace,
	}, svc)
	if err != nil {
		// Service doesn't exist, return empty
		return ""
	}

	// List all ingresses in the controller's namespace
	ingressList := &networkingv1.IngressList{}
	if err := r.List(ctx, ingressList, client.InNamespace(controllerNamespace)); err != nil {
		return ""
	}

	// Find ingress that points to rollout-dashboard service
	var baseURL string
	for i := range ingressList.Items {
		ingress := &ingressList.Items[i]

		// Check default backend first
		if ingress.Spec.DefaultBackend != nil && ingress.Spec.DefaultBackend.Service != nil {
			if ingress.Spec.DefaultBackend.Service.Name == "rollout-dashboard" {
				// For default backend, we need at least one rule with a host
				// If no rules, we can't construct a URL
				if len(ingress.Spec.Rules) > 0 && ingress.Spec.Rules[0].Host != "" {
					scheme := "https"
					if len(ingress.Spec.TLS) == 0 {
						scheme = "http"
					}
					baseURL = fmt.Sprintf("%s://%s", scheme, ingress.Spec.Rules[0].Host)
					break
				}
			}
		}

		// Check rules and paths
		for _, rule := range ingress.Spec.Rules {
			if rule.Host == "" {
				continue
			}

			// Check if any path in this rule points to rollout-dashboard service
			for _, path := range rule.HTTP.Paths {
				if path.Backend.Service != nil && path.Backend.Service.Name == "rollout-dashboard" {
					// Construct base URL
					scheme := "https"
					if len(ingress.Spec.TLS) == 0 {
						scheme = "http"
					}
					baseURL = fmt.Sprintf("%s://%s", scheme, rule.Host)
					// Note: We ignore the ingress path and use our own path instead
					break
				}
			}
			if baseURL != "" {
				break
			}
		}
		if baseURL != "" {
			break
		}
	}

	if baseURL == "" {
		// TODO: Add Gateway API support if needed
		// For now, only Ingress is supported
		return ""
	}

	// Append the path: /rollouts/<githubDeploymentNamespace>/<githubDeploymentName>
	path := fmt.Sprintf("/rollouts/%s/%s", githubDeploymentNamespace, githubDeploymentName)
	return fmt.Sprintf("%s%s", baseURL, path)
}

// extractDeploymentPayload extracts the deployment payload from a GitHub deployment
func (r *GitHubDeploymentReconciler) extractDeploymentPayload(dep *github.Deployment) *deploymentPayload {
	if len(dep.Payload) == 0 {
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

	return &payload
}

// extractDeploymentKey extracts the deployment key from a GitHub deployment's payload and environment
func (r *GitHubDeploymentReconciler) extractDeploymentKey(dep *github.Deployment) *deploymentKey {
	if dep.Environment == nil {
		return nil
	}

	payload := r.extractDeploymentPayload(dep)
	if payload == nil {
		return nil
	}

	return &deploymentKey{
		ID:          payload.ID,
		Environment: *dep.Environment,
	}
}

// syncDeploymentHistory ensures a GitHub deployment exists for each rollout history entry
// and posts a DeploymentStatus matching each entry's bake status. It returns the latest
// deployment's ID and URL for status bookkeeping on the CR, and a map of version -> deployment info
// for versions currently in history.
func (r *GitHubDeploymentReconciler) syncDeploymentHistory(ctx context.Context, gh *github.Client, ghd *kuberikv1alpha1.GitHubDeployment, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, map[string]versionDeploymentInfo, error) {
	// Parse repository
	parts := strings.Split(ghd.Spec.Repository, "/")
	if len(parts) != 2 {
		return nil, "", nil, fmt.Errorf("invalid repository format: %s", ghd.Spec.Repository)
	}
	owner, repo := parts[0], parts[1]

	// Map deployments by ID + environment from payload
	// We'll query deployments per history entry using ref + environment for more targeted queries
	keyToDeployment := make(map[deploymentKey]*github.Deployment)

	// Track deployment statuses for versions in history (current environment)
	versionDeployments := make(map[string]versionDeploymentInfo)

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

		// Format environment as "deploymentName:environment" for GitHub
		formattedEnv := formatDeploymentEnvironment(ghd.Spec.DeploymentName, ghd.Spec.Environment)

		// Create key for this history entry using ID + formatted environment
		key := deploymentKey{
			ID:          historyID,
			Environment: formattedEnv,
		}

		// Check if we already found this deployment in a previous iteration
		dep := keyToDeployment[key]

		// If not found, query deployments for this specific ref + environment + task
		// Using task (deployment name) helps differentiate different service deployments
		if dep == nil {
			task := formatDeploymentTask(ghd.Spec.DeploymentName)
			deployments, _, err := gh.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
				Ref:         ref,
				Task:        task,
				Environment: formattedEnv,
			})
			if err != nil {
				return nil, "", nil, fmt.Errorf("failed to list deployments for ref %s: %w", ref, err)
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
			payload := deploymentPayload{
				ID:           historyID,
				Dependencies: ghd.Spec.Dependencies,
			}
			payloadJSON, err := json.Marshal(payload)
			if err != nil {
				return nil, "", nil, fmt.Errorf("failed to marshal deployment payload: %w", err)
			}

			// Format task as "deploy:<name>" and environment as "deploymentName:environment"
			task := formatDeploymentTask(ghd.Spec.DeploymentName)
			formattedEnv := formatDeploymentEnvironment(ghd.Spec.DeploymentName, ghd.Spec.Environment)
			req := &github.DeploymentRequest{
				Ref:                   &ref,
				Task:                  &task,
				Environment:           &formattedEnv,
				Description:           h.Message,
				RequiredContexts:      &ghd.Spec.RequiredContexts,
				ProductionEnvironment: github.Bool(ghd.Spec.Environment == "production"),
				AutoMerge:             github.Bool(false),
				Payload:               payloadJSON,
			}
			created, _, err := gh.Repositories.CreateDeployment(ctx, owner, repo, req)
			if err != nil {
				return nil, "", nil, fmt.Errorf("failed to create GitHub deployment: %w", err)
			}
			dep = created
			keyToDeployment[key] = dep
		}

		// Determine desired GH status for this history entry
		ghState, ghDesc := mapBakeToGitHubState(h.BakeStatus)

		// Check all statuses to see if we've already created this exact status
		// This prevents duplicate statuses like: pending -> success -> pending
		statuses, _, err := gh.Repositories.ListDeploymentStatuses(ctx, owner, repo, dep.GetID(), &github.ListOptions{})
		statusExists := false
		latestStatus := ghState // Default to the desired state
		if err == nil && len(statuses) > 0 {
			// Statuses are ordered newest first, so the first one is the latest
			if statuses[0].State != nil {
				latestStatus = *statuses[0].State
			}
			for _, status := range statuses {
				if status.State != nil && *status.State == ghState && status.Description != nil && *status.Description == ghDesc {
					// This exact status already exists, skip creating it again
					statusExists = true
					break
				}
			}
		}

		if !statusExists {
			if err := r.createDeploymentStatus(ctx, gh, ghd, dep.GetID(), ghState, ghDesc); err != nil {
				return nil, "", nil, err
			}
			// After creating, the latest status is the one we just created
			latestStatus = ghState
		}

		// Track this version's deployment info (only for versions in history)
		versionDeployments[ref] = versionDeploymentInfo{
			Status:        latestStatus,
			DeploymentID:  dep.ID,
			DeploymentURL: dep.GetURL(),
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
		return nil, "", nil, fmt.Errorf("no valid history entry found with both ID and revision")
	}

	latestID := fmt.Sprintf("%d", *latest.ID)
	formattedEnv := formatDeploymentEnvironment(ghd.Spec.DeploymentName, ghd.Spec.Environment)
	latestKey := deploymentKey{
		ID:          latestID,
		Environment: formattedEnv,
	}

	if dep := keyToDeployment[latestKey]; dep != nil {
		return dep.ID, dep.GetURL(), versionDeployments, nil
	}

	// This shouldn't happen if we processed history correctly, but handle it
	return nil, "", nil, fmt.Errorf("failed to find or create deployment for ID %s and environment %s", latestID, ghd.Spec.Environment)
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

// applyRolloutGateDesiredState applies the desired state to a RolloutGate based on the GitHubDeployment spec.
// This function sets all fields that should be managed by this controller, whether creating or updating.
func (r *GitHubDeploymentReconciler) applyRolloutGateDesiredState(rolloutGate *kuberikrolloutv1alpha1.RolloutGate, githubDeployment *kuberikv1alpha1.GitHubDeployment) error {
	// Set annotations
	prettyName := "Dependencies not ready yet"
	var description string
	if len(githubDeployment.Spec.Dependencies) > 0 {
		description = fmt.Sprintf("This gate is passing only for those versions that have been successfully deployed to all environments that are specified as dependencies (%s).", strings.Join(githubDeployment.Spec.Dependencies, ", "))
	} else {
		description = "This gate is passing only for those versions that have been successfully deployed to all environments that are specified as dependencies."
	}

	if rolloutGate.Annotations == nil {
		rolloutGate.Annotations = make(map[string]string)
	}
	rolloutGate.Annotations["gate.kuberik.com/pretty-name"] = prettyName
	rolloutGate.Annotations["gate.kuberik.com/description"] = description

	// Set spec
	rolloutGate.Spec.RolloutRef = &githubDeployment.Spec.RolloutRef

	// Set owner reference
	if err := ctrl.SetControllerReference(githubDeployment, rolloutGate, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	return nil
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
		}

		// Apply desired state
		if err := r.applyRolloutGateDesiredState(rolloutGate, githubDeployment); err != nil {
			return err
		}

		if err := r.Create(ctx, rolloutGate); err != nil {
			return fmt.Errorf("failed to create RolloutGate: %w", err)
		}
		existingRolloutGate = rolloutGate
	} else {
		// RolloutGate exists, apply desired state and update if needed
		// Store original state for comparison
		originalAnnotations := make(map[string]string)
		if existingRolloutGate.Annotations != nil {
			for k, v := range existingRolloutGate.Annotations {
				originalAnnotations[k] = v
			}
		}
		var originalRolloutRef *corev1.LocalObjectReference
		if existingRolloutGate.Spec.RolloutRef != nil {
			originalRolloutRef = &corev1.LocalObjectReference{
				Name: existingRolloutGate.Spec.RolloutRef.Name,
			}
		}
		originalOwnerRefs := make([]metav1.OwnerReference, len(existingRolloutGate.OwnerReferences))
		copy(originalOwnerRefs, existingRolloutGate.OwnerReferences)

		// Apply desired state
		if err := r.applyRolloutGateDesiredState(existingRolloutGate, githubDeployment); err != nil {
			return err
		}

		// Check if update is needed by comparing current state with original
		needsUpdate := false

		// Check annotations
		if len(originalAnnotations) != len(existingRolloutGate.Annotations) {
			needsUpdate = true
		} else {
			for k, v := range existingRolloutGate.Annotations {
				if originalAnnotations[k] != v {
					needsUpdate = true
					break
				}
			}
		}

		// Check RolloutRef
		if originalRolloutRef == nil || existingRolloutGate.Spec.RolloutRef == nil {
			if originalRolloutRef != existingRolloutGate.Spec.RolloutRef {
				needsUpdate = true
			}
		} else if originalRolloutRef.Name != existingRolloutGate.Spec.RolloutRef.Name {
			needsUpdate = true
		}

		// Check owner references (SetControllerReference may have modified them)
		if len(originalOwnerRefs) != len(existingRolloutGate.OwnerReferences) {
			needsUpdate = true
		} else {
			for i, ref := range existingRolloutGate.OwnerReferences {
				if i >= len(originalOwnerRefs) || ref.UID != originalOwnerRefs[i].UID {
					needsUpdate = true
					break
				}
			}
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
// It also tracks deployment statuses and environment info for all environments (not just dependencies)
func (r *GitHubDeploymentReconciler) updateAllowedVersionsFromDependencies(ctx context.Context, githubDeployment *kuberikv1alpha1.GitHubDeployment, client *github.Client) error {
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

	// Build set of relevant versions: versions in current environment's history OR in release candidates
	relevantVersions := make(map[string]bool)
	for _, h := range rollout.Status.History {
		if h.Version.Revision != nil && *h.Version.Revision != "" {
			relevantVersions[*h.Version.Revision] = true
		}
	}
	for _, candidate := range rollout.Status.ReleaseCandidates {
		if candidate.Revision != nil && *candidate.Revision != "" {
			relevantVersions[*candidate.Revision] = true
		}
	}

	// Query all deployments with the same task to discover all environments
	task := formatDeploymentTask(githubDeployment.Spec.DeploymentName)
	allDeployments, _, err := client.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
		Task: task,
	})
	if err != nil {
		return fmt.Errorf("failed to list all deployments: %w", err)
	}

	// Group deployments by environment and extract environment names
	envDeployments := make(map[string][]*github.Deployment) // environment -> deployments
	environments := make(map[string]bool)
	for _, deployment := range allDeployments {
		if deployment.Environment == nil {
			continue
		}
		// Extract environment name from formatted environment (e.g., "deploymentName/environment" -> "environment")
		env := *deployment.Environment
		// The environment format is "deploymentName/environment", so split and take the last part
		envParts := strings.Split(env, "/")
		if len(envParts) > 0 {
			envName := envParts[len(envParts)-1]
			environments[envName] = true
			envDeployments[envName] = append(envDeployments[envName], deployment)
		}
	}

	// Initialize as empty slice (not nil) when dependencies are set
	// This ensures allowedVersions is set to empty array instead of nil
	allowedTags := []string{}
	seenTags := make(map[string]bool) // Avoid duplicates

	// Track deployment statuses for all environments
	allEnvDeployments := make(map[string]map[string]versionDeploymentInfo) // environment -> version -> deployment info
	// Track environment info (environment URL and dependencies) for all environments
	environmentInfos := make(map[string]struct {
		EnvironmentURL string
		Dependencies   []string
	})

	// Process each environment
	for envName, deployments := range envDeployments {
		if len(deployments) == 0 {
			continue
		}

		// Initialize deployment map for this environment
		if allEnvDeployments[envName] == nil {
			allEnvDeployments[envName] = make(map[string]versionDeploymentInfo)
		}

		// Track the latest environment URL and dependencies (first deployment in list is latest)
		latestDeployment := deployments[0]
		envInfo := struct {
			EnvironmentURL string
			Dependencies   []string
		}{Dependencies: []string{}}

		// Extract dependencies from the latest deployment's payload
		payload := r.extractDeploymentPayload(latestDeployment)
		if payload != nil && len(payload.Dependencies) > 0 {
			envInfo.Dependencies = payload.Dependencies
		}

		// Extract environment URL from the latest deployment's statuses
		statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, owner, repo, latestDeployment.GetID(), nil)
		if err == nil && len(statuses) > 0 {
			// Statuses are ordered newest first, so the first one has the latest environment URL
			if statuses[0].EnvironmentURL != nil && *statuses[0].EnvironmentURL != "" {
				envInfo.EnvironmentURL = *statuses[0].EnvironmentURL
			}
		}

		environmentInfos[envName] = envInfo

		// Check each deployment for success status
		for _, deployment := range deployments {
			if deployment.Ref == nil {
				continue
			}
			version := *deployment.Ref

			// Only track versions that are relevant (in history or release candidates)
			if !relevantVersions[version] {
				continue
			}

			// Get deployment statuses
			statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, owner, repo, deployment.GetID(), nil)
			if err != nil {
				continue
			}

			// Get the latest status (statuses are ordered newest first)
			latestStatus := "pending" // Default
			for _, status := range statuses {
				if status.State != nil {
					latestStatus = *status.State
					break
				}
			}

			// Track this version's deployment info (without URL for non-current environments)
			allEnvDeployments[envName][version] = versionDeploymentInfo{
				Status:        latestStatus,
				DeploymentID:  deployment.ID,
				DeploymentURL: "", // Don't store URL per version for non-current environments
			}

			// Check if any status is success for allowed versions (only for dependencies)
			if len(githubDeployment.Spec.Dependencies) > 0 {
				isDependency := false
				for _, dep := range githubDeployment.Spec.Dependencies {
					if envName == dep {
						isDependency = true
						break
					}
				}
				if isDependency {
					for _, status := range statuses {
						if status.State != nil && *status.State == "success" {
							// Match revision to tag in releaseCandidates
							if tag, found := revisionToTag[version]; found {
								// Only add if not already seen
								if !seenTags[tag] {
									allowedTags = append(allowedTags, tag)
									seenTags[tag] = true
								}
							}
							break
						}
					}
				}
			}
		}
	}

	// Update allowed versions on RolloutGate (only if dependencies are set)
	if len(githubDeployment.Spec.Dependencies) == 0 {
		// No dependencies, skip RolloutGate update but still update environment statuses
	} else if githubDeployment.Status.RolloutGateRef == nil {
		return nil
	} else {

		rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
		err = r.Get(ctx, types.NamespacedName{
			Name:      githubDeployment.Status.RolloutGateRef.Name,
			Namespace: githubDeployment.Namespace,
		}, rolloutGate)
		if err != nil {
			return fmt.Errorf("failed to get RolloutGate: %w", err)
		}

		// Check if allowedVersions need to be updated
		// When dependencies are set, we must ensure allowedVersions is set to empty array instead of nil
		needsUpdate := false
		if rolloutGate.Spec.AllowedVersions == nil {
			// If currently nil and dependencies are set, we need to set it to empty array
			needsUpdate = true
		} else {
			// Compare current values with new values
			currentAllowedVersions := *rolloutGate.Spec.AllowedVersions
			if !r.slicesEqual(currentAllowedVersions, allowedTags) {
				needsUpdate = true
			}
		}

		if needsUpdate {
			rolloutGate.Spec.AllowedVersions = &allowedTags
			if err := r.Update(ctx, rolloutGate); err != nil {
				return fmt.Errorf("failed to update RolloutGate allowedVersions: %w", err)
			}
		}
	}

	// Update deployment statuses for all environments in GitHubDeployment status
	if githubDeployment.Status.DeploymentStatuses == nil {
		githubDeployment.Status.DeploymentStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
	}

	needsStatusUpdate := false

	// Start with current statuses
	statuses := githubDeployment.Status.DeploymentStatuses

	// Update statuses for each environment (excluding current environment, which is handled separately)
	currentEnv := githubDeployment.Spec.Environment
	for envName, envDeployments := range allEnvDeployments {
		// Skip current environment (handled separately in updateGitHubDeploymentStatus)
		if envName == currentEnv {
			continue
		}

		// Update or add statuses for versions that are relevant (in history or release candidates)
		for version, depInfo := range envDeployments {
			if relevantVersions[version] {
				// Don't include URL in status entries for non-current environments
				statuses = updateDeploymentStatusEntryWithInfo(statuses, envName, version, depInfo.Status, depInfo.DeploymentID, "")
			}
		}
	}

	// Clean up: remove entries for environments that no longer exist or versions that are no longer relevant
	statuses = removeDeploymentStatusEntries(statuses, func(entry kuberikv1alpha1.DeploymentStatusEntry) bool {
		// Keep current environment entries (handled separately in updateGitHubDeploymentStatus)
		if entry.Environment == currentEnv {
			return false
		}
		// Remove if environment no longer exists
		if !environments[entry.Environment] {
			return true
		}
		// Remove if version is no longer relevant
		return !relevantVersions[entry.Version]
	})

	// Update environment infos (URLs and dependencies for all environments)
	if githubDeployment.Status.EnvironmentInfos == nil {
		githubDeployment.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{}
	}

	environmentInfoList := githubDeployment.Status.EnvironmentInfos
	for envName, info := range environmentInfos {
		environmentInfoList = updateEnvironmentInfo(environmentInfoList, envName, info.EnvironmentURL, info.Dependencies)
	}

	// Clean up environment infos for environments that no longer exist
	environmentInfoList = removeEnvironmentInfos(environmentInfoList, func(entry kuberikv1alpha1.EnvironmentInfo) bool {
		return !environments[entry.Environment]
	})

	// Check if environment infos need update
	if !environmentInfosEqual(githubDeployment.Status.EnvironmentInfos, environmentInfoList) {
		githubDeployment.Status.EnvironmentInfos = environmentInfoList
		needsStatusUpdate = true
	}

	// Check if statuses need update
	if !deploymentStatusesEqual(githubDeployment.Status.DeploymentStatuses, statuses) {
		githubDeployment.Status.DeploymentStatuses = statuses
		needsStatusUpdate = true
	}

	// Check if update is needed
	if !deploymentStatusesEqual(githubDeployment.Status.DeploymentStatuses, statuses) {
		githubDeployment.Status.DeploymentStatuses = statuses
		needsStatusUpdate = true
	}

	if needsStatusUpdate {
		if err := r.Status().Update(ctx, githubDeployment); err != nil {
			return fmt.Errorf("failed to update GitHubDeployment dependency statuses: %w", err)
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

// mapsEqual compares two string maps for equality
func (r *GitHubDeploymentReconciler) mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// findDeploymentStatusEntry finds a deployment status entry by environment and version
func findDeploymentStatusEntry(statuses []kuberikv1alpha1.DeploymentStatusEntry, environment, version string) *kuberikv1alpha1.DeploymentStatusEntry {
	for i := range statuses {
		if statuses[i].Environment == environment && statuses[i].Version == version {
			return &statuses[i]
		}
	}
	return nil
}

// updateDeploymentStatusEntry updates or adds a deployment status entry
func updateDeploymentStatusEntry(statuses []kuberikv1alpha1.DeploymentStatusEntry, environment, version, status string) []kuberikv1alpha1.DeploymentStatusEntry {
	return updateDeploymentStatusEntryWithInfo(statuses, environment, version, status, nil, "")
}

// updateDeploymentStatusEntryWithInfo updates or adds a deployment status entry with full deployment info
func updateDeploymentStatusEntryWithInfo(statuses []kuberikv1alpha1.DeploymentStatusEntry, environment, version, status string, deploymentID *int64, deploymentURL string) []kuberikv1alpha1.DeploymentStatusEntry {
	// Find existing entry
	for i := range statuses {
		if statuses[i].Environment == environment && statuses[i].Version == version {
			statuses[i].Status = status
			statuses[i].DeploymentID = deploymentID
			statuses[i].DeploymentURL = deploymentURL
			return statuses
		}
	}
	// Add new entry
	return append(statuses, kuberikv1alpha1.DeploymentStatusEntry{
		Environment:   environment,
		Version:       version,
		Status:        status,
		DeploymentID:  deploymentID,
		DeploymentURL: deploymentURL,
	})
}

// removeDeploymentStatusEntries removes all entries matching the given filter function
func removeDeploymentStatusEntries(statuses []kuberikv1alpha1.DeploymentStatusEntry, shouldRemove func(kuberikv1alpha1.DeploymentStatusEntry) bool) []kuberikv1alpha1.DeploymentStatusEntry {
	result := make([]kuberikv1alpha1.DeploymentStatusEntry, 0, len(statuses))
	for _, entry := range statuses {
		if !shouldRemove(entry) {
			result = append(result, entry)
		}
	}
	return result
}

// getDeploymentStatusesByEnvironment returns all status entries for a given environment
func getDeploymentStatusesByEnvironment(statuses []kuberikv1alpha1.DeploymentStatusEntry, environment string) map[string]string {
	result := make(map[string]string)
	for _, entry := range statuses {
		if entry.Environment == environment {
			result[entry.Version] = entry.Status
		}
	}
	return result
}

// deploymentStatusesEqual compares two deployment status lists for equality
func deploymentStatusesEqual(a, b []kuberikv1alpha1.DeploymentStatusEntry) bool {
	if len(a) != len(b) {
		return false
	}
	// Create maps for easier comparison
	aMap := make(map[string]string) // "env:version" -> status
	bMap := make(map[string]string)
	for _, entry := range a {
		key := entry.Environment + ":" + entry.Version
		aMap[key] = entry.Status
	}
	for _, entry := range b {
		key := entry.Environment + ":" + entry.Version
		bMap[key] = entry.Status
	}
	if len(aMap) != len(bMap) {
		return false
	}
	for k, v := range aMap {
		if bMap[k] != v {
			return false
		}
	}
	return true
}

// findEnvironmentInfo finds an environment info entry by environment name
func findEnvironmentInfo(infos []kuberikv1alpha1.EnvironmentInfo, environment string) *kuberikv1alpha1.EnvironmentInfo {
	for i := range infos {
		if infos[i].Environment == environment {
			return &infos[i]
		}
	}
	return nil
}

// updateEnvironmentInfo updates or adds an environment info entry
func updateEnvironmentInfo(infos []kuberikv1alpha1.EnvironmentInfo, environment, environmentURL string, dependencies []string) []kuberikv1alpha1.EnvironmentInfo {
	// Find existing entry
	for i := range infos {
		if infos[i].Environment == environment {
			infos[i].EnvironmentURL = environmentURL
			infos[i].Dependencies = dependencies
			return infos
		}
	}
	// Add new entry
	return append(infos, kuberikv1alpha1.EnvironmentInfo{
		Environment:    environment,
		EnvironmentURL: environmentURL,
		Dependencies:   dependencies,
	})
}

// removeEnvironmentInfos removes entries matching the given filter function
func removeEnvironmentInfos(infos []kuberikv1alpha1.EnvironmentInfo, shouldRemove func(kuberikv1alpha1.EnvironmentInfo) bool) []kuberikv1alpha1.EnvironmentInfo {
	result := make([]kuberikv1alpha1.EnvironmentInfo, 0, len(infos))
	for _, entry := range infos {
		if !shouldRemove(entry) {
			result = append(result, entry)
		}
	}
	return result
}

// environmentInfosEqual compares two environment info lists for equality
func environmentInfosEqual(a, b []kuberikv1alpha1.EnvironmentInfo) bool {
	if len(a) != len(b) {
		return false
	}
	// Create maps for easier comparison
	aMap := make(map[string]struct {
		EnvironmentURL string
		Dependencies   []string
	})
	bMap := make(map[string]struct {
		EnvironmentURL string
		Dependencies   []string
	})
	for _, entry := range a {
		aMap[entry.Environment] = struct {
			EnvironmentURL string
			Dependencies   []string
		}{entry.EnvironmentURL, entry.Dependencies}
	}
	for _, entry := range b {
		bMap[entry.Environment] = struct {
			EnvironmentURL string
			Dependencies   []string
		}{entry.EnvironmentURL, entry.Dependencies}
	}
	if len(aMap) != len(bMap) {
		return false
	}
	for k, v := range aMap {
		bv, exists := bMap[k]
		if !exists {
			return false
		}
		if v.EnvironmentURL != bv.EnvironmentURL {
			return false
		}
		// Compare dependencies
		if len(v.Dependencies) != len(bv.Dependencies) {
			return false
		}
		depsMap := make(map[string]bool)
		for _, dep := range v.Dependencies {
			depsMap[dep] = true
		}
		for _, dep := range bv.Dependencies {
			if !depsMap[dep] {
				return false
			}
		}
	}
	return true
}

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

	kuberikv1alpha1 "github.com/kuberik/deployment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// deploymentPayload represents the payload stored in GitHub deployments for mapping
type deploymentPayload struct {
	ID           string                                  `json:"id"`                     // Stored as string in JSON for consistency
	Relationship *kuberikv1alpha1.DeploymentRelationship `json:"relationship,omitempty"` // Relationship for this environment
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

// GitHubDeploymentReconciler reconciles a Deployment object for GitHub backend
type GitHubDeploymentReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	CacheTransport http.RoundTripper
}

// +kubebuilder:rbac:groups=deployments.kuberik.com,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=deployments.kuberik.com,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=deployments.kuberik.com,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitHubDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Deployment instance
	deployment := &kuberikv1alpha1.Deployment{}
	err := r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to get Deployment")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Validate backend
	if deployment.Spec.BackendConfig.Backend != "github" {
		return ctrl.Result{}, fmt.Errorf("unsupported backend: %s", deployment.Spec.BackendConfig.Backend)
	}

	// Get the referenced Rollout to get the current version
	rollout, err := r.getReferencedRollout(ctx, deployment)
	if err != nil {
		log.Error(err, "Failed to get referenced Rollout")
		return ctrl.Result{}, err
	}

	// Get GitHub client
	githubClient, err := r.getGitHubClient(ctx, deployment)
	if err != nil {
		log.Error(err, "Failed to get GitHub client")
		return ctrl.Result{}, err
	}

	// Sync entire rollout history with GitHub deployments and statuses
	deploymentID, deploymentURL, currentEnvDeployments, err := r.syncDeploymentHistory(ctx, githubClient, deployment, rollout)
	if err != nil {
		log.Error(err, "Failed to sync deployment history")
		return ctrl.Result{}, err
	}
	// If no valid history entry found, requeue and wait
	if deploymentID == nil {
		log.Info("No valid history entry found, requeuing")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Update Deployment status
	if err := r.updateDeploymentStatus(ctx, deployment, deploymentID, deploymentURL, rollout, currentEnvDeployments); err != nil {
		log.Error(err, "Failed to update Deployment status")
		return ctrl.Result{}, err
	}

	// Create or update RolloutGate
	if err := r.createOrUpdateRolloutGate(ctx, deployment); err != nil {
		log.Error(err, "Failed to create or update RolloutGate")
		return ctrl.Result{}, err
	}

	// Check relationships and update allowed versions on RolloutGate
	if err := r.updateAllowedVersionsFromRelationships(ctx, deployment, githubClient); err != nil {
		log.Error(err, "Failed to update allowed versions from relationships")
		return ctrl.Result{}, err
	}

	// Requeue at configured interval (default 1 minute) to keep status updated
	requeueInterval := time.Minute // default
	if deployment.Spec.RequeueInterval != "" {
		parsed, err := time.ParseDuration(deployment.Spec.RequeueInterval)
		if err != nil {
			log.Error(err, "Invalid RequeueInterval, using default", "interval", deployment.Spec.RequeueInterval)
		} else {
			requeueInterval = parsed
		}
	}
	return ctrl.Result{RequeueAfter: requeueInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitHubDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikv1alpha1.Deployment{}).
		Watches(
			&kuberikrolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.rolloutToDeployment),
		).
		Named("deployment").
		Complete(r)
}

// rolloutToDeployment maps a Rollout to all Deployments that reference it
func (r *GitHubDeploymentReconciler) rolloutToDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout := obj.(*kuberikrolloutv1alpha1.Rollout)
	requests := []reconcile.Request{}

	// List all Deployments in the same namespace
	deploymentList := &kuberikv1alpha1.DeploymentList{}
	if err := r.List(ctx, deploymentList, client.InNamespace(rollout.Namespace)); err != nil {
		return requests
	}

	// Find all Deployments that reference this Rollout
	for i := range deploymentList.Items {
		deployment := &deploymentList.Items[i]
		if deployment.Spec.RolloutRef.Name == rollout.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      deployment.Name,
					Namespace: deployment.Namespace,
				},
			})
		}
	}

	return requests
}

// getReferencedRollout gets the Rollout referenced by the Deployment
func (r *GitHubDeploymentReconciler) getReferencedRollout(ctx context.Context, deployment *kuberikv1alpha1.Deployment) (*kuberikrolloutv1alpha1.Rollout, error) {
	rollout := &kuberikrolloutv1alpha1.Rollout{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      deployment.Spec.RolloutRef.Name,
		Namespace: deployment.Namespace,
	}, rollout)
	if err != nil {
		return nil, fmt.Errorf("failed to get Rollout %s: %w", deployment.Spec.RolloutRef.Name, err)
	}
	return rollout, nil
}

// getGitHubClient creates a GitHub client using the token from the specified secret
// The client uses ghcache for conditional requests with caching to reduce API rate limit consumption
// ghcache automatically partitions the cache by auth header, ensuring proper token isolation
func (r *GitHubDeploymentReconciler) getGitHubClient(ctx context.Context, deployment *kuberikv1alpha1.Deployment) (*github.Client, error) {
	secretName := deployment.Spec.BackendConfig.BackendSecret
	if secretName == "" {
		secretName = "github-token" // Default secret name
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: deployment.Namespace,
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
func (r *GitHubDeploymentReconciler) createDeploymentStatus(ctx context.Context, client *github.Client, deployment *kuberikv1alpha1.Deployment, deploymentID int64, state string, description string) error {
	owner, repo, err := parseProject(deployment.Spec.BackendConfig.Project)
	if err != nil {
		return err
	}

	deploymentName := deployment.Spec.DeploymentName
	if err := validateDeploymentName(deploymentName); err != nil {
		return err
	}
	// Format environment as "deploymentName/environment" for GitHub
	formattedEnv := formatDeploymentEnvironment(deploymentName, deployment.Spec.Environment)

	// Get rollout-dashboard URL from ingress/gateway in controller's namespace
	// The URL will include the path /rollouts/<namespace>/<name>
	environmentURL := r.getRolloutDashboardURL(ctx, deployment.Namespace, deployment.Name)

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
	_, _, createErr := client.Repositories.CreateDeploymentStatus(ctx, owner, repo, deploymentID, statusRequest)
	if createErr != nil {
		return fmt.Errorf("failed to create deployment status: %w", createErr)
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

// updateDeploymentStatus updates the Deployment status with deployment information
func (r *GitHubDeploymentReconciler) updateDeploymentStatus(ctx context.Context, deployment *kuberikv1alpha1.Deployment, deploymentID *int64, deploymentURL string, rollout *kuberikrolloutv1alpha1.Rollout, currentEnvDeployments map[string]versionDeploymentInfo) error {
	needsUpdate := false

	// Update deployment ID
	if deployment.Status.DeploymentID == nil || *deployment.Status.DeploymentID != *deploymentID {
		deployment.Status.DeploymentID = deploymentID
		needsUpdate = true
	}

	// Update deployment URL
	if deployment.Status.DeploymentURL != deploymentURL {
		deployment.Status.DeploymentURL = deploymentURL
		needsUpdate = true
	}

	// Update last sync time
	now := metav1.Now()
	if deployment.Status.LastSyncTime == nil || deployment.Status.LastSyncTime.Before(&now) {
		deployment.Status.LastSyncTime = &now
		needsUpdate = true
	}

	// Update current version
	currentVersion := r.getCurrentVersionFromRollout(rollout)
	if currentVersion != nil && deployment.Status.CurrentVersion != *currentVersion {
		deployment.Status.CurrentVersion = *currentVersion
		needsUpdate = true
	}

	// Update deployment statuses for current environment
	// Only keep versions that are still in history
	if deployment.Status.DeploymentStatuses == nil {
		deployment.Status.DeploymentStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
	}

	currentEnv := deployment.Spec.Environment

	// Build set of versions that should be kept (only those in history)
	versionsInHistory := make(map[string]bool)
	for _, h := range rollout.Status.History {
		if h.Version.Revision != nil && *h.Version.Revision != "" {
			versionsInHistory[*h.Version.Revision] = true
		}
	}

	// Remove all entries for current environment that are no longer in history
	statuses := deployment.Status.DeploymentStatuses
	statuses = removeDeploymentStatusEntries(statuses, func(entry kuberikv1alpha1.DeploymentStatusEntry) bool {
		return entry.Environment == currentEnv && !versionsInHistory[entry.Version]
	})

	// Update or add statuses for versions in history
	for version, depInfo := range currentEnvDeployments {
		if versionsInHistory[version] {
			statuses = updateDeploymentStatusEntryWithInfo(statuses, currentEnv, version, depInfo.Status, depInfo.DeploymentID, depInfo.DeploymentURL)
		}
	}

	// Check if update is needed
	if !deploymentStatusesEqual(deployment.Status.DeploymentStatuses, statuses) {
		deployment.Status.DeploymentStatuses = statuses
		needsUpdate = true
	}

	if needsUpdate {
		return r.Status().Update(ctx, deployment)
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

// parseProject parses the project string into owner and repo
func parseProject(project string) (owner, repo string, err error) {
	parts := strings.Split(project, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid project format: %s", project)
	}
	return parts[0], parts[1], nil
}

// validateDeploymentName validates that the deployment name starts with "kuberik" prefix
func validateDeploymentName(deploymentName string) error {
	if !strings.HasPrefix(deploymentName, "kuberik") {
		return fmt.Errorf("GitHub deployment name must start with 'kuberik' prefix, got: %s", deploymentName)
	}
	return nil
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
// The URL will have the path /rollouts/<deploymentNamespace>/<deploymentName> appended.
func (r *GitHubDeploymentReconciler) getRolloutDashboardURL(ctx context.Context, deploymentNamespace, deploymentName string) string {
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

	// Append the path: /rollouts/<deploymentNamespace>/<deploymentName>
	path := fmt.Sprintf("/rollouts/%s/%s", deploymentNamespace, deploymentName)
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
func (r *GitHubDeploymentReconciler) syncDeploymentHistory(ctx context.Context, gh *github.Client, deployment *kuberikv1alpha1.Deployment, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, map[string]versionDeploymentInfo, error) {
	owner, repo, err := parseProject(deployment.Spec.BackendConfig.Project)
	if err != nil {
		return nil, "", nil, err
	}

	deploymentName := deployment.Spec.DeploymentName
	if err := validateDeploymentName(deploymentName); err != nil {
		return nil, "", nil, err
	}
	formattedEnv := formatDeploymentEnvironment(deploymentName, deployment.Spec.Environment)
	task := formatDeploymentTask(deploymentName)

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
				Relationship: deployment.Spec.Relationship,
			}
			payloadJSON, err := json.Marshal(payload)
			if err != nil {
				return nil, "", nil, fmt.Errorf("failed to marshal deployment payload: %w", err)
			}

			req := &github.DeploymentRequest{
				Ref:                   &ref,
				Task:                  &task,
				Environment:           &formattedEnv,
				Description:           h.Message,
				ProductionEnvironment: github.Bool(deployment.Spec.Environment == "production"),
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
			if err := r.createDeploymentStatus(ctx, gh, deployment, dep.GetID(), ghState, ghDesc); err != nil {
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
		// No valid history entry - return nil values to indicate nothing to sync
		// This is not an error condition, just means we should wait for a valid entry
		return nil, "", nil, nil
	}

	latestID := fmt.Sprintf("%d", *latest.ID)
	latestKey := deploymentKey{
		ID:          latestID,
		Environment: formattedEnv,
	}

	if dep := keyToDeployment[latestKey]; dep != nil {
		return dep.ID, dep.GetURL(), versionDeployments, nil
	}

	// This shouldn't happen if we processed history correctly, but handle it
	return nil, "", nil, fmt.Errorf("failed to find or create deployment for ID %s and environment %s", latestID, deployment.Spec.Environment)
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

// applyRolloutGateDesiredState applies the desired state to a RolloutGate based on the Deployment spec.
// This function sets all fields that should be managed by this controller, whether creating or updating.
func (r *GitHubDeploymentReconciler) applyRolloutGateDesiredState(rolloutGate *kuberikrolloutv1alpha1.RolloutGate, deployment *kuberikv1alpha1.Deployment) error {
	// Set annotations
	prettyName := "Relationship not ready yet"
	var description string
	if deployment.Spec.Relationship != nil {
		relType := "deployed"
		switch deployment.Spec.Relationship.Type {
		case kuberikv1alpha1.RelationshipTypeAfter:
			relType = "deployed after"
		case kuberikv1alpha1.RelationshipTypeParallel:
			relType = "deployed in parallel with"
		}
		description = fmt.Sprintf("This gate is passing only for those versions that have been successfully %s the %s environment.", relType, deployment.Spec.Relationship.Environment)
	} else {
		description = "This gate is passing only for those versions that have been successfully deployed to the related environment."
	}

	if rolloutGate.Annotations == nil {
		rolloutGate.Annotations = make(map[string]string)
	}
	rolloutGate.Annotations["gate.kuberik.com/pretty-name"] = prettyName
	rolloutGate.Annotations["gate.kuberik.com/description"] = description

	// Set spec
	rolloutGate.Spec.RolloutRef = &deployment.Spec.RolloutRef

	// Set owner reference
	if err := ctrl.SetControllerReference(deployment, rolloutGate, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	return nil
}

// createOrUpdateRolloutGate creates or updates the RolloutGate based on the Deployment spec
func (r *GitHubDeploymentReconciler) createOrUpdateRolloutGate(ctx context.Context, deployment *kuberikv1alpha1.Deployment) error {
	// List all RolloutGates in the namespace to find one owned by this Deployment
	rolloutGateList := &kuberikrolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, rolloutGateList, client.InNamespace(deployment.Namespace)); err != nil {
		return fmt.Errorf("failed to list RolloutGates: %w", err)
	}

	// Find existing RolloutGate owned by this Deployment
	var existingRolloutGate *kuberikrolloutv1alpha1.RolloutGate
	for i := range rolloutGateList.Items {
		gate := &rolloutGateList.Items[i]
		for _, ownerRef := range gate.OwnerReferences {
			if ownerRef.Kind == "Deployment" &&
				ownerRef.APIVersion == kuberikv1alpha1.GroupVersion.String() &&
				ownerRef.Name == deployment.Name {
				// If UID is set, it must match; otherwise match by name and kind only
				if ownerRef.UID == "" || ownerRef.UID == deployment.UID {
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
				Namespace:    deployment.Namespace,
			},
		}

		// Apply desired state
		if err := r.applyRolloutGateDesiredState(rolloutGate, deployment); err != nil {
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
		if err := r.applyRolloutGateDesiredState(existingRolloutGate, deployment); err != nil {
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

	// Update the RolloutGateRef in Deployment status
	rolloutGateRef := &corev1.LocalObjectReference{Name: existingRolloutGate.Name}
	if deployment.Status.RolloutGateRef == nil || deployment.Status.RolloutGateRef.Name != rolloutGateRef.Name {
		deployment.Status.RolloutGateRef = rolloutGateRef
		return r.Status().Update(ctx, deployment)
	}

	return nil
}

// updateAllowedVersionsFromRelationships checks deployment relationships and updates allowed versions on RolloutGate
// It also tracks deployment statuses and environment info for all related environments
// Version relevance is determined by relationships: we track versions that are relevant to the current environment
// based on "After" and "Parallel" relationships
func (r *GitHubDeploymentReconciler) updateAllowedVersionsFromRelationships(ctx context.Context, deployment *kuberikv1alpha1.Deployment, client *github.Client) error {
	// Get the referenced Rollout to access releaseCandidates
	rollout, err := r.getReferencedRollout(ctx, deployment)
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

	owner, repo, err := parseProject(deployment.Spec.BackendConfig.Project)
	if err != nil {
		return err
	}

	deploymentName := deployment.Spec.DeploymentName
	if err := validateDeploymentName(deploymentName); err != nil {
		return err
	}

	// Build relationship graph to determine relevant environments
	// Relevant environments are: current environment + all environments related to it (directly or transitively)
	relevantEnvironments := make(map[string]bool)
	currentEnv := deployment.Spec.Environment
	relevantEnvironments[currentEnv] = true

	// Build a map of environment -> relationships from all discovered environments
	// We'll populate this as we discover environments
	envRelationships := make(map[string]*kuberikv1alpha1.DeploymentRelationship)

	// Query all deployments with the same task to discover all environments
	task := formatDeploymentTask(deploymentName)
	allDeployments, _, err := client.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
		Task: task,
	})
	if err != nil {
		return fmt.Errorf("failed to list all deployments: %w", err)
	}

	// Group deployments by environment and extract environment names
	envDeployments := make(map[string][]*github.Deployment) // environment -> deployments
	environments := make(map[string]bool)
	for _, d := range allDeployments {
		if d.Environment == nil {
			continue
		}
		// Extract environment name from formatted environment (e.g., "kuberik-deployment/environment" -> "environment")
		env := *d.Environment
		// The environment format is "deploymentName/environment", so split and take the last part
		envParts := strings.Split(env, "/")
		if len(envParts) > 0 {
			envName := envParts[len(envParts)-1]
			environments[envName] = true
			envDeployments[envName] = append(envDeployments[envName], d)

			// Extract relationship from payload
			payload := r.extractDeploymentPayload(d)
			if payload != nil && payload.Relationship != nil {
				envRelationships[envName] = payload.Relationship
			}
		}
	}

	// Build set of relevant environments based on relationships
	// Start with current environment and traverse relationships
	visited := make(map[string]bool)
	var traverseRelationships func(env string)
	traverseRelationships = func(env string) {
		if visited[env] {
			return
		}
		visited[env] = true
		relevantEnvironments[env] = true

		// Find all environments that have a relationship pointing to this environment
		for otherEnv, rel := range envRelationships {
			if rel != nil && rel.Environment == env {
				traverseRelationships(otherEnv)
			}
		}

		// Also traverse environments that this environment relates to
		if rel, exists := envRelationships[env]; exists && rel != nil {
			traverseRelationships(rel.Environment)
		}
	}
	traverseRelationships(currentEnv)

	// Build set of relevant versions: versions that appear in any relevant environment
	relevantVersions := make(map[string]bool)
	for envName, deployments := range envDeployments {
		if !relevantEnvironments[envName] {
			continue
		}
		for _, d := range deployments {
			if d.Ref != nil && *d.Ref != "" {
				relevantVersions[*d.Ref] = true
			}
		}
	}

	// Initialize as empty slice (not nil) when relationship is set
	// This ensures allowedVersions is set to empty array instead of nil
	allowedTags := []string{}
	seenTags := make(map[string]bool) // Avoid duplicates

	// Track deployment statuses for all environments
	allEnvDeployments := make(map[string]map[string]versionDeploymentInfo) // environment -> version -> deployment info
	// Track environment info (environment URL and relationship) for all environments
	environmentInfos := make(map[string]struct {
		EnvironmentURL string
		Relationship   *kuberikv1alpha1.DeploymentRelationship
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

		// Track the latest environment URL and relationship (first deployment in list is latest)
		latestDeployment := deployments[0]
		envInfo := struct {
			EnvironmentURL string
			Relationship   *kuberikv1alpha1.DeploymentRelationship
		}{}

		// Extract relationship from the latest deployment's payload
		payload := r.extractDeploymentPayload(latestDeployment)
		if payload != nil && payload.Relationship != nil {
			envInfo.Relationship = payload.Relationship
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
		for _, d := range deployments {
			if d.Ref == nil {
				continue
			}
			version := *d.Ref

			// Only track versions that are relevant
			if !relevantVersions[version] {
				continue
			}

			// Get deployment statuses
			statuses, _, err := client.Repositories.ListDeploymentStatuses(ctx, owner, repo, d.GetID(), nil)
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
				Status:       latestStatus,
				DeploymentID: d.ID,
				// DeploymentURL is only set for current environment in updateDeploymentStatus
			}

			// Check if any status is success for allowed versions (only for related environments)
			if deployment.Spec.Relationship != nil {
				relatedEnv := deployment.Spec.Relationship.Environment
				if envName == relatedEnv {
					// For "After" relationship: version must be successfully deployed in related environment
					// For "Parallel" relationship: version must be successfully deployed in related environment
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

	// Update allowed versions on RolloutGate (only if relationship is set)
	if deployment.Spec.Relationship == nil {
		// No relationship, skip RolloutGate update but still update environment statuses
	} else if deployment.Status.RolloutGateRef == nil {
		return nil
	} else {
		rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
		err = r.Get(ctx, types.NamespacedName{
			Name:      deployment.Status.RolloutGateRef.Name,
			Namespace: deployment.Namespace,
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

	// Update deployment statuses for all environments in Deployment status
	if deployment.Status.DeploymentStatuses == nil {
		deployment.Status.DeploymentStatuses = []kuberikv1alpha1.DeploymentStatusEntry{}
	}

	needsStatusUpdate := false

	// Start with current statuses
	statuses := deployment.Status.DeploymentStatuses

	// Update statuses for each environment (excluding current environment, which is handled separately)
	for envName, envDeployments := range allEnvDeployments {
		// Skip current environment (handled separately in updateDeploymentStatus)
		if envName == currentEnv {
			continue
		}

		// Only track statuses for relevant environments
		if !relevantEnvironments[envName] {
			continue
		}

		// Update or add statuses for versions that are relevant
		for version, depInfo := range envDeployments {
			if relevantVersions[version] {
				// Don't include URL in status entries for non-current environments
				statuses = updateDeploymentStatusEntryWithInfo(statuses, envName, version, depInfo.Status, depInfo.DeploymentID, "")
			}
		}
	}

	// Clean up: remove entries for environments that are no longer relevant or versions that are no longer relevant
	statuses = removeDeploymentStatusEntries(statuses, func(entry kuberikv1alpha1.DeploymentStatusEntry) bool {
		// Keep current environment entries (handled separately in updateDeploymentStatus)
		if entry.Environment == currentEnv {
			return false
		}
		// Remove if environment is no longer relevant
		if !relevantEnvironments[entry.Environment] {
			return true
		}
		// Remove if version is no longer relevant
		return !relevantVersions[entry.Version]
	})

	// Update environment infos (URLs and relationships for all environments)
	if deployment.Status.EnvironmentInfos == nil {
		deployment.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{}
	}

	environmentInfoList := deployment.Status.EnvironmentInfos
	for envName, info := range environmentInfos {
		environmentInfoList = updateEnvironmentInfoWithRelationship(environmentInfoList, envName, info.EnvironmentURL, info.Relationship)
	}

	// Clean up environment infos for environments that are no longer relevant
	environmentInfoList = removeEnvironmentInfos(environmentInfoList, func(entry kuberikv1alpha1.EnvironmentInfo) bool {
		return !relevantEnvironments[entry.Environment]
	})

	// Check if environment infos need update
	if !environmentInfosEqual(deployment.Status.EnvironmentInfos, environmentInfoList) {
		deployment.Status.EnvironmentInfos = environmentInfoList
		needsStatusUpdate = true
	}

	// Check if statuses need update
	if !deploymentStatusesEqual(deployment.Status.DeploymentStatuses, statuses) {
		deployment.Status.DeploymentStatuses = statuses
		needsStatusUpdate = true
	}

	if needsStatusUpdate {
		if err := r.Status().Update(ctx, deployment); err != nil {
			return fmt.Errorf("failed to update Deployment relationship statuses: %w", err)
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

// updateEnvironmentInfoWithRelationship updates or adds an environment info entry
func updateEnvironmentInfoWithRelationship(infos []kuberikv1alpha1.EnvironmentInfo, environment, environmentURL string, relationship *kuberikv1alpha1.DeploymentRelationship) []kuberikv1alpha1.EnvironmentInfo {
	// Find existing entry
	for i := range infos {
		if infos[i].Environment == environment {
			infos[i].EnvironmentURL = environmentURL
			infos[i].Relationship = relationship
			return infos
		}
	}
	// Add new entry
	return append(infos, kuberikv1alpha1.EnvironmentInfo{
		Environment:    environment,
		EnvironmentURL: environmentURL,
		Relationship:   relationship,
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
		Relationship   *kuberikv1alpha1.DeploymentRelationship
	})
	bMap := make(map[string]struct {
		EnvironmentURL string
		Relationship   *kuberikv1alpha1.DeploymentRelationship
	})
	for _, entry := range a {
		aMap[entry.Environment] = struct {
			EnvironmentURL string
			Relationship   *kuberikv1alpha1.DeploymentRelationship
		}{entry.EnvironmentURL, entry.Relationship}
	}
	for _, entry := range b {
		bMap[entry.Environment] = struct {
			EnvironmentURL string
			Relationship   *kuberikv1alpha1.DeploymentRelationship
		}{entry.EnvironmentURL, entry.Relationship}
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
		// Compare relationships
		if (v.Relationship == nil) != (bv.Relationship == nil) {
			return false
		}
		if v.Relationship != nil && bv.Relationship != nil {
			if v.Relationship.Environment != bv.Relationship.Environment {
				return false
			}
			if v.Relationship.Type != bv.Relationship.Type {
				return false
			}
		}
	}
	return true
}

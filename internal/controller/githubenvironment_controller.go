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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kuberikv1alpha1 "github.com/kuberik/environment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// deploymentPayload represents the payload stored in GitHub deployments for mapping
type deploymentPayload struct {
	ID                     string                                         `json:"id"`                               // Stored as string in JSON for consistency
	Relationship           *kuberikv1alpha1.EnvironmentRelationship       `json:"relationship,omitempty"`           // Relationship for this environment
	DeploymentHistoryEntry *kuberikrolloutv1alpha1.DeploymentHistoryEntry `json:"deploymentHistoryEntry,omitempty"` // The entire deployment history entry
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
	HistoryEntry  *kuberikrolloutv1alpha1.DeploymentHistoryEntry
}

// GitHubEnvironmentReconciler reconciles an Environment object for GitHub backend
type GitHubEnvironmentReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	CacheTransport http.RoundTripper
}

// +kubebuilder:rbac:groups=environments.kuberik.com,resources=environments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=environments.kuberik.com,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=environments.kuberik.com,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups=kuberik.com,resources=rolloutgates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kuberik.com,resources=rollouts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *GitHubEnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Environment instance
	deployment := &kuberikv1alpha1.Environment{}
	err := r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "Failed to get Environment")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Validate backend
	if deployment.Spec.Backend.Type != "github" {
		return ctrl.Result{}, fmt.Errorf("unsupported backend: %s", deployment.Spec.Backend.Type)
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
	deploymentID, deploymentURL, _, err := r.syncDeploymentHistory(ctx, githubClient, deployment, rollout)
	if err != nil {
		log.Error(err, "Failed to sync deployment history")
		return ctrl.Result{}, err
	}
	// If no valid history entry found, requeue and wait
	if deploymentID == nil {
		log.Info("No valid history entry found, requeuing")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Update Environment status
	if err := r.updateEnvironmentStatus(ctx, deployment, deploymentID, deploymentURL, rollout); err != nil {
		log.Error(err, "Failed to update Environment status")
		return ctrl.Result{}, err
	}

	// Create or update RolloutGate
	if err := r.createOrUpdateRolloutGate(ctx, deployment); err != nil {
		log.Error(err, "Failed to create or update RolloutGate")
		return ctrl.Result{}, err
	}

	// Build relationship graph
	graphData, err := r.buildRelationshipGraph(ctx, deployment, githubClient)
	if err != nil {
		log.Error(err, "Failed to build relationship graph")
		return ctrl.Result{}, err
	}

	// Update allowed versions on RolloutGate based on relationships
	if err := r.updateAllowedVersionsFromRelationships(ctx, deployment, graphData); err != nil {
		log.Error(err, "Failed to update allowed versions from relationships")
		return ctrl.Result{}, err
	}

	// Update deployment statuses for related environments
	if err := r.updateDeploymentStatusesForRelatedEnvironments(ctx, deployment, graphData); err != nil {
		log.Error(err, "Failed to update deployment statuses for related environments")
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
func (r *GitHubEnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kuberikv1alpha1.Environment{}).
		Watches(
			&kuberikrolloutv1alpha1.Rollout{},
			handler.EnqueueRequestsFromMapFunc(r.rolloutToEnvironment),
		).
		Named("environment").
		Complete(r)
}

// rolloutToEnvironment maps a Rollout to all Environments that reference it
func (r *GitHubEnvironmentReconciler) rolloutToEnvironment(ctx context.Context, obj client.Object) []reconcile.Request {
	rollout := obj.(*kuberikrolloutv1alpha1.Rollout)
	requests := []reconcile.Request{}

	// List all Environments in the same namespace
	deploymentList := &kuberikv1alpha1.EnvironmentList{}
	if err := r.List(ctx, deploymentList, client.InNamespace(rollout.Namespace)); err != nil {
		return requests
	}

	// Find all Environments that reference this Rollout
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

// getReferencedRollout gets the Rollout referenced by the Environment
func (r *GitHubEnvironmentReconciler) getReferencedRollout(ctx context.Context, deployment *kuberikv1alpha1.Environment) (*kuberikrolloutv1alpha1.Rollout, error) {
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
func (r *GitHubEnvironmentReconciler) getGitHubClient(ctx context.Context, deployment *kuberikv1alpha1.Environment) (*github.Client, error) {
	secretName := deployment.Spec.Backend.Secret
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

// createDeploymentStatus creates a GitHub deployment status for the given environment
func (r *GitHubEnvironmentReconciler) createDeploymentStatus(ctx context.Context, client *github.Client, environment *kuberikv1alpha1.Environment, deploymentID int64, state string, description string) error {
	owner, repo, err := parseProject(environment.Spec.Backend.Project)
	if err != nil {
		return err
	}

	// Ensure deployment name has kuberik prefix for GitHub
	deploymentName := ensureKuberikPrefix(environment.Spec.Name)
	// Format environment as "deploymentName/environment" for GitHub
	formattedEnv := formatDeploymentEnvironment(deploymentName, environment.Spec.Environment)

	// Get rollout-dashboard URL from ingress/gateway in controller's namespace
	// The URL will include the path /rollouts/<namespace>/<name>
	environmentURL := r.getRolloutDashboardURL(ctx, environment.Namespace, environment.Name)

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
func (r *GitHubEnvironmentReconciler) getCurrentVersionFromRollout(rollout *kuberikrolloutv1alpha1.Rollout) *string {
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

// updateEnvironmentStatus updates the Environment status with GitHub deployment information
func (r *GitHubEnvironmentReconciler) updateEnvironmentStatus(ctx context.Context, environment *kuberikv1alpha1.Environment, deploymentID *int64, deploymentURL string, rollout *kuberikrolloutv1alpha1.Rollout) error {
	needsUpdate := false

	// Update deployment ID
	if environment.Status.DeploymentID == nil || *environment.Status.DeploymentID != *deploymentID {
		environment.Status.DeploymentID = deploymentID
		needsUpdate = true
	}

	// Update deployment URL
	if environment.Status.DeploymentURL != deploymentURL {
		environment.Status.DeploymentURL = deploymentURL
		needsUpdate = true
	}

	// Update current version
	currentVersion := r.getCurrentVersionFromRollout(rollout)
	if currentVersion != nil && environment.Status.CurrentVersion != *currentVersion {
		environment.Status.CurrentVersion = *currentVersion
		needsUpdate = true
	}

	// Update history for current environment from rollout
	currentEnv := environment.Spec.Environment

	// Build history entries for current environment from rollout
	newHistory := make([]kuberikrolloutv1alpha1.DeploymentHistoryEntry, 0, len(rollout.Status.History))
	for _, entry := range rollout.Status.History {
		// Only include entries with valid revision
		if entry.Version.Revision != nil && *entry.Version.Revision != "" {
			newHistory = append(newHistory, entry)
		}
	}

	// Update or create EnvironmentInfo for current environment with history
	if environment.Status.EnvironmentInfos == nil {
		environment.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{}
	}

	// Find or create EnvironmentInfo for current environment
	envInfoIndex := -1
	for i, info := range environment.Status.EnvironmentInfos {
		if info.Environment == currentEnv {
			envInfoIndex = i
			break
		}
	}

	if envInfoIndex == -1 {
		// Create new EnvironmentInfo
		environment.Status.EnvironmentInfos = append(environment.Status.EnvironmentInfos, kuberikv1alpha1.EnvironmentInfo{
			Environment: currentEnv,
			History:     newHistory,
		})
		needsUpdate = true
	} else {
		// Update existing EnvironmentInfo
		envInfo := &environment.Status.EnvironmentInfos[envInfoIndex]
		if !historyEntriesEqual(envInfo.History, newHistory) {
			envInfo.History = newHistory
			needsUpdate = true
		}
	}

	if needsUpdate {
		// Update last status change time only when status actually changes
		now := metav1.Now()
		environment.Status.LastStatusChangeTime = &now
		return r.Status().Update(ctx, environment)
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

// ensureKuberikPrefix ensures the deployment name starts with "kuberik" prefix, adding it if missing
func ensureKuberikPrefix(deploymentName string) string {
	if strings.HasPrefix(deploymentName, "kuberik/") {
		return deploymentName
	}
	return "kuberik/" + deploymentName
}

// getControllerNamespace gets the namespace where the controller is running.
// It reads from the service account namespace file, or falls back to environment variable.
func (r *GitHubEnvironmentReconciler) getControllerNamespace() string {
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
func (r *GitHubEnvironmentReconciler) getRolloutDashboardURL(ctx context.Context, deploymentNamespace, deploymentName string) string {
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
		// Check Gateway API (HTTPRoute) if Ingress not found
		httpRouteList := &gatewayv1.HTTPRouteList{}
		if err := r.List(ctx, httpRouteList, client.InNamespace(controllerNamespace)); err != nil {
			return ""
		}

		// Find HTTPRoute that points to rollout-dashboard service
		for i := range httpRouteList.Items {
			httpRoute := &httpRouteList.Items[i]

			// Check if any rule references rollout-dashboard service
			for _, rule := range httpRoute.Spec.Rules {
				for _, backendRef := range rule.BackendRefs {
					// Check if this backendRef points to rollout-dashboard service
					// Default to Service kind if not specified
					kind := gatewayv1.Kind("Service")
					if backendRef.Kind != nil {
						kind = *backendRef.Kind
					}
					if kind != gatewayv1.Kind("Service") {
						continue
					}

					// Check namespace - default to HTTPRoute's namespace if not specified
					backendNamespace := controllerNamespace
					if backendRef.Namespace != nil {
						backendNamespace = string(*backendRef.Namespace)
					}

					// Only check if backend is in the same namespace as the service
					if backendNamespace == controllerNamespace && backendRef.Name == gatewayv1.ObjectName("rollout-dashboard") {
						// Found HTTPRoute pointing to rollout-dashboard
						// Now we need to get the Gateway to find the hostname
						if len(httpRoute.Spec.ParentRefs) == 0 {
							continue
						}

						// Get the first parent Gateway
						parentRef := httpRoute.Spec.ParentRefs[0]
						gatewayName := string(parentRef.Name)
						gatewayNamespace := controllerNamespace
						if parentRef.Namespace != nil {
							gatewayNamespace = string(*parentRef.Namespace)
						}

						gateway := &gatewayv1.Gateway{}
						if err := r.Get(ctx, types.NamespacedName{
							Name:      gatewayName,
							Namespace: gatewayNamespace,
						}, gateway); err != nil {
							continue
						}

						// Find a listener with a hostname
						for _, listener := range gateway.Spec.Listeners {
							if listener.Hostname != nil && *listener.Hostname != "" {
								scheme := "https"
								if listener.Protocol == gatewayv1.HTTPProtocolType {
									scheme = "http"
								}
								baseURL = fmt.Sprintf("%s://%s", scheme, string(*listener.Hostname))
								break
							}
						}

						if baseURL != "" {
							break
						}
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
	}

	if baseURL == "" {
		return ""
	}

	// Append the path: /rollouts/<deploymentNamespace>/<deploymentName>
	path := fmt.Sprintf("/rollouts/%s/%s", deploymentNamespace, deploymentName)
	return fmt.Sprintf("%s%s", baseURL, path)
}

// extractDeploymentPayload extracts the deployment payload from a GitHub deployment
func (r *GitHubEnvironmentReconciler) extractDeploymentPayload(dep *github.Deployment) *deploymentPayload {
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
func (r *GitHubEnvironmentReconciler) extractDeploymentKey(dep *github.Deployment) *deploymentKey {
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
func (r *GitHubEnvironmentReconciler) syncDeploymentHistory(ctx context.Context, gh *github.Client, environment *kuberikv1alpha1.Environment, rollout *kuberikrolloutv1alpha1.Rollout) (*int64, string, map[string]versionDeploymentInfo, error) {
	owner, repo, err := parseProject(environment.Spec.Backend.Project)
	if err != nil {
		return nil, "", nil, err
	}

	// Ensure deployment name has kuberik prefix for GitHub
	deploymentName := ensureKuberikPrefix(environment.Spec.Name)
	formattedEnv := formatDeploymentEnvironment(deploymentName, environment.Spec.Environment)
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
				ID:                     historyID,
				Relationship:           environment.Spec.Relationship,
				DeploymentHistoryEntry: &h,
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
				ProductionEnvironment: github.Bool(environment.Spec.Environment == "production"),
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
			if err := r.createDeploymentStatus(ctx, gh, environment, dep.GetID(), ghState, ghDesc); err != nil {
				return nil, "", nil, err
			}
			// After creating, the latest status is the one we just created
			latestStatus = ghState
		}

		// Track this version's deployment info (only for versions in history)
		// Create a copy of the history entry to avoid pointer issues
		historyEntryCopy := h
		versionDeployments[ref] = versionDeploymentInfo{
			Status:        latestStatus,
			DeploymentID:  dep.ID,
			DeploymentURL: dep.GetURL(),
			HistoryEntry:  &historyEntryCopy,
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
	return nil, "", nil, fmt.Errorf("failed to find or create deployment for ID %s and environment %s", latestID, environment.Spec.Environment)
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
func (r *GitHubEnvironmentReconciler) applyRolloutGateDesiredState(rolloutGate *kuberikrolloutv1alpha1.RolloutGate, deployment *kuberikv1alpha1.Environment) error {
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

// createOrUpdateRolloutGate creates or updates the RolloutGate based on the Environment spec
func (r *GitHubEnvironmentReconciler) createOrUpdateRolloutGate(ctx context.Context, environment *kuberikv1alpha1.Environment) error {
	// List all RolloutGates in the namespace to find one owned by this Environment
	rolloutGateList := &kuberikrolloutv1alpha1.RolloutGateList{}
	if err := r.List(ctx, rolloutGateList, client.InNamespace(environment.Namespace)); err != nil {
		return fmt.Errorf("failed to list RolloutGates: %w", err)
	}

	// Find existing RolloutGate owned by this Environment
	var existingRolloutGate *kuberikrolloutv1alpha1.RolloutGate
	for i := range rolloutGateList.Items {
		gate := &rolloutGateList.Items[i]
		for _, ownerRef := range gate.OwnerReferences {
			if ownerRef.Kind == "Environment" &&
				ownerRef.APIVersion == kuberikv1alpha1.GroupVersion.String() &&
				ownerRef.Name == environment.Name {
				// If UID is set, it must match; otherwise match by name and kind only
				if ownerRef.UID == "" || ownerRef.UID == environment.UID {
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
				Namespace:    environment.Namespace,
			},
		}

		// Apply desired state
		if err := r.applyRolloutGateDesiredState(rolloutGate, environment); err != nil {
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
		if err := r.applyRolloutGateDesiredState(existingRolloutGate, environment); err != nil {
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

	// Update the RolloutGateRef in Environment status
	rolloutGateRef := &corev1.LocalObjectReference{Name: existingRolloutGate.Name}
	if environment.Status.RolloutGateRef == nil || environment.Status.RolloutGateRef.Name != rolloutGateRef.Name {
		environment.Status.RolloutGateRef = rolloutGateRef
		return r.Status().Update(ctx, environment)
	}

	return nil
}

// relationshipGraphData holds the data needed for relationship-based operations
type relationshipGraphData struct {
	relevantEnvironments map[string]bool
	relevantVersions     map[string]bool
	envDeployments       map[string][]*github.Deployment
	// envHistory maps environment name -> history entries from that environment's rollout
	envHistory map[string][]kuberikrolloutv1alpha1.DeploymentHistoryEntry
	// envDeploymentStatuses maps environment name -> version -> GitHub deployment status ("success", "failure", etc.)
	envDeploymentStatuses map[string]map[string]string
	environmentInfos      map[string]struct {
		EnvironmentURL string
		Relationship   *kuberikv1alpha1.EnvironmentRelationship
	}
}

// buildRelationshipGraph discovers all environments and builds the relationship graph
// It first updates environment infos, then fetches history from rollouts for each environment
func (r *GitHubEnvironmentReconciler) buildRelationshipGraph(ctx context.Context, environment *kuberikv1alpha1.Environment, ghClient *github.Client) (*relationshipGraphData, error) {
	owner, repo, err := parseProject(environment.Spec.Backend.Project)
	if err != nil {
		return nil, err
	}

	// Ensure deployment name has kuberik prefix for GitHub
	deploymentName := ensureKuberikPrefix(environment.Spec.Name)

	// Build relationship graph to determine relevant environments
	// Relevant environments are: current environment + all environments related to it (directly or transitively)
	relevantEnvironments := make(map[string]bool)
	currentEnv := environment.Spec.Environment
	relevantEnvironments[currentEnv] = true

	// Build a map of environment -> relationships from all discovered environments
	envRelationships := make(map[string]*kuberikv1alpha1.EnvironmentRelationship)

	// Query all deployments with the same task to discover all environments
	task := formatDeploymentTask(deploymentName)
	allDeployments, _, err := ghClient.Repositories.ListDeployments(ctx, owner, repo, &github.DeploymentsListOptions{
		Task: task,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list all deployments: %w", err)
	}

	// Group deployments by environment and extract environment names
	envDeployments := make(map[string][]*github.Deployment) // environment -> deployments
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
			envDeployments[envName] = append(envDeployments[envName], d)

			// Extract relationship from payload
			payload := r.extractDeploymentPayload(d)
			if payload != nil && payload.Relationship != nil {
				envRelationships[envName] = payload.Relationship
			}
		}
	}

	// Also discover environments from Environment resources (not just GitHub deployments)
	// This ensures we find related environments even if they don't have GitHub deployments yet
	environmentList := &kuberikv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environmentList, client.InNamespace(environment.Namespace)); err == nil {
		for i := range environmentList.Items {
			env := &environmentList.Items[i]
			// Match by deployment name
			if env.Spec.Name != environment.Spec.Name {
				continue
			}
			if env.Spec.Environment == "" {
				continue
			}
			envName := env.Spec.Environment

			// Add to discovered environments
			if envDeployments[envName] == nil {
				envDeployments[envName] = []*github.Deployment{}
			}

			// Extract relationship from Environment spec
			if env.Spec.Relationship != nil {
				envRelationships[envName] = env.Spec.Relationship
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

	// Track environment info (environment URL and relationship) for all environments
	environmentInfos := make(map[string]struct {
		EnvironmentURL string
		Relationship   *kuberikv1alpha1.EnvironmentRelationship
	})

	// First, update environment infos from GitHub deployments
	for envName, deployments := range envDeployments {
		if len(deployments) == 0 {
			continue
		}

		// Track the latest environment URL and relationship (first deployment in list is latest)
		latestDeployment := deployments[0]
		envInfo := struct {
			EnvironmentURL string
			Relationship   *kuberikv1alpha1.EnvironmentRelationship
		}{}

		// Extract relationship from the latest deployment's payload
		payload := r.extractDeploymentPayload(latestDeployment)
		if payload != nil && payload.Relationship != nil {
			envInfo.Relationship = payload.Relationship
		}

		// Extract environment URL from the latest deployment's statuses
		statuses, _, err := ghClient.Repositories.ListDeploymentStatuses(ctx, owner, repo, latestDeployment.GetID(), nil)
		if err == nil && len(statuses) > 0 {
			// Statuses are ordered newest first, so the first one has the latest environment URL
			if statuses[0].EnvironmentURL != nil && *statuses[0].EnvironmentURL != "" {
				envInfo.EnvironmentURL = *statuses[0].EnvironmentURL
			}
		}

		environmentInfos[envName] = envInfo
	}

	// Also add environment infos for environments discovered from Environment resources (without GitHub deployments)
	// Reuse environmentList from above
	for i := range environmentList.Items {
		env := &environmentList.Items[i]
		if env.Spec.Name != environment.Spec.Name {
			continue
		}
		if env.Spec.Environment == "" {
			continue
		}
		envName := env.Spec.Environment

		// Only add if not already set from GitHub deployments and if it's a relevant environment
		if _, exists := environmentInfos[envName]; !exists && relevantEnvironments[envName] {
			envInfo := struct {
				EnvironmentURL string
				Relationship   *kuberikv1alpha1.EnvironmentRelationship
			}{}
			if env.Spec.Relationship != nil {
				envInfo.Relationship = env.Spec.Relationship
			}
			environmentInfos[envName] = envInfo
		} else if exists && environmentInfos[envName].Relationship == nil && env.Spec.Relationship != nil {
			// Update relationship from Environment spec if not set from GitHub
			envInfo := environmentInfos[envName]
			envInfo.Relationship = env.Spec.Relationship
			environmentInfos[envName] = envInfo
		}
	}

	// Now fetch history from rollouts for each environment
	// First, try to get history from Environment resources (most accurate)
	envHistory := make(map[string][]kuberikrolloutv1alpha1.DeploymentHistoryEntry)
	// Re-fetch environmentList to ensure we have the latest (in case it was modified)
	environmentList = &kuberikv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environmentList, client.InNamespace(environment.Namespace)); err == nil {
		for i := range environmentList.Items {
			env := &environmentList.Items[i]
			// Check if this environment matches one of our discovered environments
			// Match by environment name and deployment name
			if env.Spec.Environment == "" || env.Spec.Name != environment.Spec.Name {
				continue
			}
			envName := env.Spec.Environment
			if !relevantEnvironments[envName] {
				continue
			}

			// Get the rollout for this environment
			rollout, err := r.getReferencedRollout(ctx, env)
			if err != nil {
				// Skip if we can't get the rollout
				continue
			}

			// Extract history entries from the rollout
			historyEntries := make([]kuberikrolloutv1alpha1.DeploymentHistoryEntry, 0, len(rollout.Status.History))
			for _, entry := range rollout.Status.History {
				// Only include entries with valid revision
				if entry.Version.Revision != nil && *entry.Version.Revision != "" {
					historyEntries = append(historyEntries, entry)
				}
			}
			envHistory[envName] = historyEntries
		}
	}

	// Track deployment statuses from GitHub for environments without Environment resources
	envDeploymentStatuses := make(map[string]map[string]string) // environment -> version -> status

	// For environments without Environment resources, extract history from GitHub deployment payloads
	for envName, deployments := range envDeployments {
		// Skip if we already have history from Environment resource
		if _, exists := envHistory[envName]; exists {
			continue
		}
		// Only process relevant environments
		if !relevantEnvironments[envName] {
			continue
		}

		// Initialize status map for this environment
		if envDeploymentStatuses[envName] == nil {
			envDeploymentStatuses[envName] = make(map[string]string)
		}

		// Collect history entries from deployment payloads
		historyEntries := make([]kuberikrolloutv1alpha1.DeploymentHistoryEntry, 0)
		seenIDs := make(map[int64]bool) // Avoid duplicates
		for _, d := range deployments {
			payload := r.extractDeploymentPayload(d)
			if payload != nil && payload.DeploymentHistoryEntry != nil {
				entry := payload.DeploymentHistoryEntry
				// Only include entries with valid revision and ID
				if entry.ID != nil && entry.Version.Revision != nil && *entry.Version.Revision != "" {
					if !seenIDs[*entry.ID] {
						historyEntries = append(historyEntries, *entry)
						seenIDs[*entry.ID] = true
					}
				}
			}

			// Also track GitHub deployment status for this version
			if d.Ref != nil && *d.Ref != "" {
				statuses, _, err := ghClient.Repositories.ListDeploymentStatuses(ctx, owner, repo, d.GetID(), nil)
				if err == nil && len(statuses) > 0 {
					// Statuses are ordered newest first
					if statuses[0].State != nil {
						envDeploymentStatuses[envName][*d.Ref] = *statuses[0].State
					}
				}
			}
		}
		if len(historyEntries) > 0 {
			envHistory[envName] = historyEntries
		}
	}

	// Build set of relevant versions: versions that appear in any relevant environment's history
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

	return &relationshipGraphData{
		relevantEnvironments:  relevantEnvironments,
		relevantVersions:      relevantVersions,
		envDeployments:        envDeployments,
		envHistory:            envHistory,
		envDeploymentStatuses: envDeploymentStatuses,
		environmentInfos:      environmentInfos,
	}, nil
}

// updateAllowedVersionsFromRelationships checks environment relationships and updates allowed versions on RolloutGate
// Version relevance is determined by relationships: we track versions that are relevant to the current environment
// based on "After" and "Parallel" relationships
func (r *GitHubEnvironmentReconciler) updateAllowedVersionsFromRelationships(ctx context.Context, environment *kuberikv1alpha1.Environment, graphData *relationshipGraphData) error {
	// Get the referenced Rollout to access releaseCandidates
	rollout, err := r.getReferencedRollout(ctx, environment)
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

	// Initialize as empty slice (not nil) when relationship is set
	// This ensures allowedVersions is set to empty array instead of nil
	allowedTags := []string{}
	seenTags := make(map[string]bool) // Avoid duplicates

	// Find allowed versions from related environment history
	if environment.Spec.Relationship != nil {
		relatedEnv := environment.Spec.Relationship.Environment
		// Check if related environment has history
		if relatedEnvHistory, exists := graphData.envHistory[relatedEnv]; exists {
			for _, entry := range relatedEnvHistory {
				if entry.Version.Revision == nil || *entry.Version.Revision == "" {
					continue
				}
				revision := *entry.Version.Revision

				// Check if bake status is succeeded (for entries from rollouts)
				bakeSucceeded := entry.BakeStatus != nil && *entry.BakeStatus == kuberikrolloutv1alpha1.BakeStatusSucceeded

				// For entries from GitHub deployments (without bake status), check GitHub deployment status
				if !bakeSucceeded && graphData.envDeploymentStatuses != nil {
					if envStatuses, exists := graphData.envDeploymentStatuses[relatedEnv]; exists {
						if status, exists := envStatuses[revision]; exists && status == "success" {
							bakeSucceeded = true
						}
					}
				}

				if bakeSucceeded {
					// Match revision to tag in releaseCandidates
					if tag, found := revisionToTag[revision]; found {
						// Only add if not already seen
						if !seenTags[tag] {
							allowedTags = append(allowedTags, tag)
							seenTags[tag] = true
						}
					}
				}
			}
		}
	}

	// Update allowed versions on RolloutGate (only if relationship is set)
	if environment.Spec.Relationship == nil {
		// No relationship, skip RolloutGate update
		return nil
	} else if environment.Status.RolloutGateRef == nil {
		return nil
	} else {
		rolloutGate := &kuberikrolloutv1alpha1.RolloutGate{}
		err = r.Get(ctx, types.NamespacedName{
			Name:      environment.Status.RolloutGateRef.Name,
			Namespace: environment.Namespace,
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

	return nil
}

// updateDeploymentStatusesForRelatedEnvironments updates history for all related environments
func (r *GitHubEnvironmentReconciler) updateDeploymentStatusesForRelatedEnvironments(ctx context.Context, environment *kuberikv1alpha1.Environment, graphData *relationshipGraphData) error {
	needsStatusUpdate := false

	// Update environment infos (URLs, relationships, and history for all environments)
	if environment.Status.EnvironmentInfos == nil {
		environment.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{}
	}

	// Make a deep copy of the environment infos to avoid modifying the original slice
	// This is necessary so that environmentInfosEqual can properly detect changes
	environmentInfoList := make([]kuberikv1alpha1.EnvironmentInfo, len(environment.Status.EnvironmentInfos))
	for i := range environment.Status.EnvironmentInfos {
		environmentInfoList[i] = *environment.Status.EnvironmentInfos[i].DeepCopy()
	}

	// Update or add environment infos with history from graphData
	for envName, info := range graphData.environmentInfos {
		// Get history for this environment
		history := []kuberikrolloutv1alpha1.DeploymentHistoryEntry{}
		if envHistory, exists := graphData.envHistory[envName]; exists {
			// Include all history entries (don't filter by relevantVersions here, as that would
			// prevent new versions from being included. The relevantVersions set is built from
			// existing history, so new versions wouldn't be in it yet)
			for _, entry := range envHistory {
				if entry.Version.Revision != nil && *entry.Version.Revision != "" {
					history = append(history, entry)
				}
			}
		}

		environmentInfoList = updateEnvironmentInfoWithHistory(environmentInfoList, envName, info.EnvironmentURL, info.Relationship, history)
	}

	// Clean up environment infos for environments that are no longer relevant
	environmentInfoList = removeEnvironmentInfos(environmentInfoList, func(entry kuberikv1alpha1.EnvironmentInfo) bool {
		return !graphData.relevantEnvironments[entry.Environment]
	})

	// Check if environment infos need update
	if !environmentInfosEqual(environment.Status.EnvironmentInfos, environmentInfoList) {
		environment.Status.EnvironmentInfos = environmentInfoList
		needsStatusUpdate = true
	}

	if needsStatusUpdate {
		if err := r.Status().Update(ctx, environment); err != nil {
			return fmt.Errorf("failed to update Environment relationship statuses: %w", err)
		}
	}

	return nil
}

// slicesEqual compares two string slices for equality
func (r *GitHubEnvironmentReconciler) slicesEqual(a, b []string) bool {
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

// updateEnvironmentInfoWithRelationship updates or adds an environment info entry
func updateEnvironmentInfoWithRelationship(infos []kuberikv1alpha1.EnvironmentInfo, environment, environmentURL string, relationship *kuberikv1alpha1.EnvironmentRelationship) []kuberikv1alpha1.EnvironmentInfo {
	return updateEnvironmentInfoWithHistory(infos, environment, environmentURL, relationship, nil)
}

// updateEnvironmentInfoWithHistory updates or adds an environment info entry with history
// It always updates history if provided, even if it appears equal, to ensure we have the latest data
func updateEnvironmentInfoWithHistory(infos []kuberikv1alpha1.EnvironmentInfo, environment, environmentURL string, relationship *kuberikv1alpha1.EnvironmentRelationship, history []kuberikrolloutv1alpha1.DeploymentHistoryEntry) []kuberikv1alpha1.EnvironmentInfo {
	// Find existing entry
	for i := range infos {
		if infos[i].Environment == environment {
			infos[i].EnvironmentURL = environmentURL
			infos[i].Relationship = relationship
			if history != nil {
				// Always update history if provided to ensure we have the latest data
				// The comparison in environmentInfosEqual will detect if it actually changed
				infos[i].History = history
			}
			return infos
		}
	}
	// Add new entry
	return append(infos, kuberikv1alpha1.EnvironmentInfo{
		Environment:    environment,
		EnvironmentURL: environmentURL,
		Relationship:   relationship,
		History:        history,
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

// historyEntriesEqual compares two DeploymentHistoryEntry slices for equality
// It compares entries by ID (which uniquely identifies a history entry) and all relevant fields
func historyEntriesEqual(a, b []kuberikrolloutv1alpha1.DeploymentHistoryEntry) bool {
	if len(a) != len(b) {
		return false
	}

	// Create maps for easier comparison: key is ID (or revision if ID is not available)
	aMap := make(map[string]kuberikrolloutv1alpha1.DeploymentHistoryEntry)
	bMap := make(map[string]kuberikrolloutv1alpha1.DeploymentHistoryEntry)

	for _, entry := range a {
		key := ""
		if entry.ID != nil {
			key = fmt.Sprintf("id:%d", *entry.ID)
		} else if entry.Version.Revision != nil {
			key = fmt.Sprintf("rev:%s", *entry.Version.Revision)
		}
		if key != "" {
			aMap[key] = entry
		}
	}

	for _, entry := range b {
		key := ""
		if entry.ID != nil {
			key = fmt.Sprintf("id:%d", *entry.ID)
		} else if entry.Version.Revision != nil {
			key = fmt.Sprintf("rev:%s", *entry.Version.Revision)
		}
		if key != "" {
			bMap[key] = entry
		}
	}

	if len(aMap) != len(bMap) {
		return false
	}

	// Compare entries by key (ID or revision)
	for key, aEntry := range aMap {
		bEntry, exists := bMap[key]
		if !exists {
			return false
		}
		// Compare all relevant fields that can change
		// ID comparison
		if (aEntry.ID == nil) != (bEntry.ID == nil) {
			return false
		}
		if aEntry.ID != nil && bEntry.ID != nil && *aEntry.ID != *bEntry.ID {
			return false
		}
		// Bake status comparison
		if (aEntry.BakeStatus == nil) != (bEntry.BakeStatus == nil) {
			return false
		}
		if aEntry.BakeStatus != nil && bEntry.BakeStatus != nil && *aEntry.BakeStatus != *bEntry.BakeStatus {
			return false
		}
		// Bake status message comparison
		if (aEntry.BakeStatusMessage == nil) != (bEntry.BakeStatusMessage == nil) {
			return false
		}
		if aEntry.BakeStatusMessage != nil && bEntry.BakeStatusMessage != nil && *aEntry.BakeStatusMessage != *bEntry.BakeStatusMessage {
			return false
		}
		// Bake start time comparison
		if (aEntry.BakeStartTime == nil) != (bEntry.BakeStartTime == nil) {
			return false
		}
		if aEntry.BakeStartTime != nil && bEntry.BakeStartTime != nil && !aEntry.BakeStartTime.Equal(bEntry.BakeStartTime) {
			return false
		}
		// Bake end time comparison
		if (aEntry.BakeEndTime == nil) != (bEntry.BakeEndTime == nil) {
			return false
		}
		if aEntry.BakeEndTime != nil && bEntry.BakeEndTime != nil && !aEntry.BakeEndTime.Equal(bEntry.BakeEndTime) {
			return false
		}
	}

	return true
}

// environmentInfosEqual compares two environment info lists for equality
func environmentInfosEqual(a, b []kuberikv1alpha1.EnvironmentInfo) bool {
	if len(a) != len(b) {
		return false
	}
	// Create maps for easier comparison
	aMap := make(map[string]kuberikv1alpha1.EnvironmentInfo)
	bMap := make(map[string]kuberikv1alpha1.EnvironmentInfo)
	for _, entry := range a {
		aMap[entry.Environment] = entry
	}
	for _, entry := range b {
		bMap[entry.Environment] = entry
	}
	if len(aMap) != len(bMap) {
		return false
	}
	for k, aEntry := range aMap {
		bEntry, exists := bMap[k]
		if !exists {
			return false
		}
		if aEntry.EnvironmentURL != bEntry.EnvironmentURL {
			return false
		}
		// Compare relationships
		if (aEntry.Relationship == nil) != (bEntry.Relationship == nil) {
			return false
		}
		if aEntry.Relationship != nil && bEntry.Relationship != nil {
			if aEntry.Relationship.Environment != bEntry.Relationship.Environment {
				return false
			}
			if aEntry.Relationship.Type != bEntry.Relationship.Type {
				return false
			}
		}
		// Compare history
		if !historyEntriesEqual(aEntry.History, bEntry.History) {
			return false
		}
	}
	return true
}

// getEnvDeploymentKeys returns the keys from allEnvDeployments map for debugging
func getEnvDeploymentKeys(allEnvDeployments map[string]map[string]versionDeploymentInfo) []string {
	keys := make([]string, 0, len(allEnvDeployments))
	for k := range allEnvDeployments {
		keys = append(keys, k)
	}
	return keys
}

// getVersionKeys returns the keys from a version deployment info map for debugging
func getVersionKeys(deps map[string]versionDeploymentInfo) []string {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	return keys
}

// getEnvDeploymentKeysFromRaw returns keys from the raw envDeployments map
func getEnvDeploymentKeysFromRaw(envDeployments map[string][]*github.Deployment) []string {
	keys := make([]string, 0, len(envDeployments))
	for k := range envDeployments {
		keys = append(keys, k)
	}
	return keys
}

// getRelevantEnvKeys returns keys from relevantEnvironments map
func getRelevantEnvKeys(relevantEnvironments map[string]bool) []string {
	keys := make([]string, 0, len(relevantEnvironments))
	for k, v := range relevantEnvironments {
		if v {
			keys = append(keys, k)
		}
	}
	return keys
}

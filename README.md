# Deployment Controller

A Kubernetes controller for managing deployments across different backends. Currently implements GitHub backend integration that reports deployment status to GitHub's Deployments API and manages deployment relationships.

## Features

- **Multi-Backend Support**: Generic Deployment API that can support multiple backends (currently GitHub)
- **GitHub Deployment Integration**: Creates and manages GitHub deployments for Deployment resources
- **Relationship Management**: Manages deployment relationships between environments (after, togetherWith)
- **Status Reporting**: Reports deployment status back to GitHub Deployments API
- **Automatic RolloutGate Creation**: Automatically creates and manages RolloutGate resources

## Architecture

The controller manages Deployment resources and integrates with backend-specific implementations:

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Deployment    │    │  Deployment     │    │  GitHub Deploy  │
│   (CRD)         │───▶│  Controller    │───▶│  API            │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                │
                                ▼
                       ┌─────────────────┐
                       │  RolloutGate    │
                       │  (auto-created) │
                       └─────────────────┘
```

## Configuration

The controller uses the Deployment CRD for configuration:

```yaml
apiVersion: deployments.kuberik.com/v1alpha1
kind: Deployment
metadata:
  name: myapp-production
  namespace: default
spec:
  # Backend configuration
  backend: "github"

  # Reference to the Rollout that this Deployment manages
  rolloutRef:
    name: myapp-rollout

  # GitHub repository configuration
  repository: "myorg/myapp"
  deploymentName: "kuberik-myapp-production"
  environment: "production"
  ref: "main"

  # Relationship configuration
  relationship:
    type: "after"
    environments:
      - "staging"
      - "testing"

  requiredContexts:
    - "ci"
    - "security-scan"

  # GitHub token configuration
  githubTokenSecret: "github-token"
```

## Deployment Spec

### Required Fields

- `backend`: Backend type (currently only "github" is supported)
- `rolloutRef`: Reference to the Rollout resource
- `repository`: Repository identifier (for GitHub: "owner/repo")
- `deploymentName`: Name of the deployment (must start with "kuberik" prefix for GitHub backend)
- `environment`: Environment name

### Optional Fields

- `ref`: Git reference (branch, tag, or SHA) - defaults to the revision from Rollout history
- `relationship`: Defines relationship to other environments
  - `type`: "after" or "togetherWith"
  - `environments`: List of environment names this deployment relates to
- `requiredContexts`: List of required status check contexts
- `githubTokenSecret`: Name of the secret containing GitHub token (default: "github-token")
- `requeueInterval`: Interval for reconciliation (default: "1m")

## GitHub Token Secret

The controller requires a GitHub token to authenticate with the GitHub API. Create a secret with the token:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-token
  namespace: default
type: Opaque
data:
  token: <base64-encoded-github-token>
```

## Requirements

- The referenced `Rollout` must have deployment history with a `Revision` field in the `VersionInfo` structure
- For GitHub backend, deployment names must start with "kuberik" prefix
- If the revision is not available, the controller will requeue and wait

## How It Works

1. **Deployment Detection**: The controller watches for Deployment resources with the configured backend.

2. **Backend Validation**: The controller validates that the backend is supported (currently only "github").

3. **Rollout Reference**: The controller fetches the referenced Rollout to get the current deployment version.

4. **Version Resolution**: The controller gets the current version from the Rollout's deployment history, using the `Revision` field from `VersionInfo`.

5. **GitHub Deployment Sync**: For GitHub backend, the controller syncs the entire rollout history with GitHub deployments and statuses.

6. **Status Reporting**: The deployment status is reported back to GitHub's Deployments API.

7. **RolloutGate Management**: The controller automatically creates and manages RolloutGate resources for the Deployment.

8. **Relationship Resolution**: If relationships are specified, the controller checks deployment statuses across environments to determine allowed versions.

## Installation

### Prerequisites

- Kubernetes cluster
- kubectl configured
- GitHub token with appropriate permissions (for GitHub backend)

### Install the Controller

1. **Install CRDs**:
   ```bash
   kubectl apply -f config/crd/bases/
   ```

2. **Install the controller**:
   ```bash
   kubectl apply -k config/default/
   ```

3. **Create GitHub token secret**:
   ```bash
   kubectl apply -f config/samples/github-token-secret.yaml
   ```

4. **Create Deployment resources**:
   ```bash
   kubectl apply -k config/samples/
   ```

## Development

### Building

```bash
make build
```

### Testing

```bash
make test
```

### Running Locally

```bash
make run
```

## API Reference

### Deployment Spec

```go
type DeploymentSpec struct {
    Backend          string                  `json:"backend"`
    RolloutRef       corev1.LocalObjectReference `json:"rolloutRef"`
    Repository       string                  `json:"repository"`
    DeploymentName   string                  `json:"deploymentName"`
    Environment      string                  `json:"environment"`
    Ref              string                  `json:"ref,omitempty"`
    Relationship     *DeploymentRelationship `json:"relationship,omitempty"`
    RequiredContexts []string                `json:"requiredContexts,omitempty"`
    GitHubTokenSecret string                 `json:"githubTokenSecret,omitempty"`
    RequeueInterval  string                  `json:"requeueInterval,omitempty"`
}
```

### DeploymentRelationship

```go
type DeploymentRelationship struct {
    Type         string   `json:"type"` // "after" or "togetherWith"
    Environments []string `json:"environments"`
}
```

### Deployment Status

```go
type DeploymentStatus struct {
    DeploymentID  *int64                    `json:"deploymentId,omitempty"`
    DeploymentURL string                    `json:"deploymentUrl,omitempty"`
    Statuses      []DeploymentStatusEntry    `json:"statuses,omitempty"`
    Environments  []EnvironmentInfo         `json:"environments,omitempty"`
}
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Submit a pull request

## License

Apache 2.0

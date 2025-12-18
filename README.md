# Deployment Controller

A Kubernetes controller for managing deployments across different backends. Currently implements GitHub backend integration that reports deployment status to GitHub's Deployments API and manages deployment relationships.

## Features

- **Multi-Backend Support**: Generic Deployment API that can support multiple backends (currently GitHub)
- **GitHub Deployment Integration**: Creates and manages GitHub deployments for Deployment resources
- **Relationship Management**: Manages deployment relationships between environments (After, Parallel)
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
  # Reference to the Rollout that this Deployment manages
  rolloutRef:
    name: myapp-rollout

  # Deployment configuration
  name: "kuberik-myapp-production"
  environment: "production"
  ref: "main"

  # Relationship configuration
  relationship:
    type: "After"
    environment: "staging"

  # Backend-specific configuration
  backend:
    type: "github"
    project: "myorg/myapp"
    secret: "github-token"
```

## Deployment Spec

### Required Fields

- `rolloutRef`: Reference to the Rollout resource
- `name`: Name of the deployment (must start with "kuberik" prefix for GitHub backend)
- `backend`: Backend-specific configuration
  - `type`: Backend type (currently only "github" is supported)
  - `project`: Project identifier (for GitHub: "owner/repo")
  - `secret`: Name of the secret containing backend token (optional, default: "github-token" for GitHub)

### Optional Fields

- `environment`: Environment name (e.g., "production", "staging")
- `ref`: Git reference (branch, tag, or SHA) - defaults to the revision from Rollout history
- `relationship`: Defines relationship to other environments
  - `type`: "After" or "Parallel"
  - `environment`: Environment name this deployment relates to
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
    RolloutRef       corev1.LocalObjectReference `json:"rolloutRef"`
    Name             string                  `json:"name"`
    Environment      string                  `json:"environment,omitempty"`
    Ref              string                  `json:"ref,omitempty"`
    Relationship     *DeploymentRelationship `json:"relationship,omitempty"`
    Backend          BackendConfig           `json:"backend"`
    RequeueInterval  string                  `json:"requeueInterval,omitempty"`
}

type BackendConfig struct {
    Type             string   `json:"type"`
    Project          string   `json:"project"`
    Secret           string   `json:"secret,omitempty"`
}
```

### DeploymentRelationship

```go
type DeploymentRelationship struct {
    Environment string           `json:"environment"`
    Type        RelationshipType `json:"type"` // "After" or "Parallel"
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

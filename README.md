# GitHub RolloutGate Controller

A Kubernetes controller for managing RolloutGate resources with GitHub integration. This controller implements a GitHub gate class that reports deployment status to GitHub's Deployments API and manages deployment dependencies.

## Features

- **GitHub Deployment Integration**: Creates and manages GitHub deployments for RolloutGate resources
- **Dependency Management**: Reads GitHub deployment statuses to determine allowed versions based on successful deployments
- **Annotation-based Configuration**: Uses annotations for GitHub-specific configuration
- **Status Reporting**: Reports deployment status back to GitHub Deployments API

## Architecture

The controller integrates with the existing RolloutGate API from the rollout-controller and extends it with GitHub-specific functionality:

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   RolloutGate   │    │ GitHub Rollout  │    │ GitHub Deploy   │
│   (CRD)         │───▶│ Gate Controller │───▶│ API             │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                                │
                                ▼
                       ┌─────────────────┐
                       │ Dependency      │
                       │ Resolution      │
                       └─────────────────┘
```

## Configuration

The controller uses annotations on the RolloutGate resource for GitHub-specific configuration:

```yaml
apiVersion: kuberik.com/v1alpha1
kind: RolloutGate
metadata:
  name: github-gate
  annotations:
    kuberik.com/gate-class: "github"
    kuberik.com/github-repo: "myorg/myapp"
    kuberik.com/deployment-name: "myapp-production"
    kuberik.com/environment: "production"
    kuberik.com/ref: "main"
    kuberik.com/description: "Production deployment"
    kuberik.com/auto-merge: "true"
    kuberik.com/dependencies: "myapp-staging,myapp-testing"
    kuberik.com/required-contexts: "ci,security-scan"
spec:
  rolloutRef:
    name: myapp-rollout
  passing: true
```

## Required Annotations

- `kuberik.com/gate-class`: Must be set to "github" to enable GitHub gate functionality
- `kuberik.com/github-repo`: GitHub repository in format "owner/repo"
- `kuberik.com/deployment-name`: Name of the current deployment

## Optional Annotations

- `kuberik.com/environment`: GitHub deployment environment (default: "production")
- `kuberik.com/ref`: Git reference (branch, tag, or SHA)
- `kuberik.com/description`: Description for the deployment
- `kuberik.com/auto-merge`: Whether to automatically merge the deployment
- `kuberik.com/dependencies`: Comma-separated list of deployment dependencies
- `kuberik.com/required-contexts`: Comma-separated list of required status check contexts
- `kuberik.com/github-token`: Name of the secret containing GitHub token (default: "github-token")

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
- If the revision is not available, the controller will fail with an error message

## How It Works

1. **Gate Detection**: The controller identifies RolloutGate resources with `kuberik.com/gate-class: "github"` annotation.

2. **Configuration Extraction**: GitHub configuration is extracted from annotations.

3. **GitHub Client**: A GitHub client is created using the token from the specified secret.

4. **Version Resolution**: The controller gets the current version from the referenced Rollout's deployment history, using the `Revision` field from `VersionInfo`. If the revision is not available, the controller will error out as it's required for GitHub deployments.

5. **Deployment Creation**: A GitHub deployment is created or updated using the current version from the rollout.

6. **Status Reporting**: The deployment status is reported back to GitHub's Deployments API.

7. **Dependency Resolution**: If dependencies are specified, the controller checks GitHub deployment statuses to determine allowed versions based on successful deployments.

8. **Version Management**: Allowed versions are updated based on successful dependency deployments.

## Installation

### Prerequisites

- Kubernetes cluster
- kubectl configured
- GitHub token with appropriate permissions

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

4. **Create RolloutGate resources**:
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

### RolloutGate Spec

```go
type RolloutGateSpec struct {
    RolloutRef *corev1.LocalObjectReference `json:"rolloutRef"`
    Passing    *bool                         `json:"passing,omitempty"`
    AllowedVersions *[]string                `json:"allowedVersions,omitempty"`
    GitHub     *GitHubConfig                 `json:"github,omitempty"`
}
```

### GitHubConfig

```go
type GitHubConfig struct {
    Repository        string   `json:"repository"`
    DeploymentName    string   `json:"deploymentName"`
    Dependencies      []string `json:"dependencies,omitempty"`
    Environment       string   `json:"environment,omitempty"`
    Ref               string   `json:"ref,omitempty"`
    Description       string   `json:"description,omitempty"`
    RequiredContexts  []string `json:"requiredContexts,omitempty"`
}
```

### RolloutGate Status

```go
type RolloutGateStatus struct {
    GitHubDeploymentID *int64            `json:"githubDeploymentId,omitempty"`
    GitHubDeploymentURL string            `json:"githubDeploymentUrl,omitempty"`
    LastSyncTime        *metav1.Time      `json:"lastSyncTime,omitempty"`
    Conditions          []metav1.Condition `json:"conditions,omitempty"`
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

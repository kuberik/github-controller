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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RelationshipType defines the type of relationship between environments
type RelationshipType string

const (
	// RelationshipTypeAfter means this environment deploys after the related environment
	RelationshipTypeAfter RelationshipType = "After"
	// RelationshipTypeParallel means this environment deploys in parallel with the related environment
	RelationshipTypeParallel RelationshipType = "Parallel"
)

// EnvironmentRelationship defines a relationship to another environment
type EnvironmentRelationship struct {
	// Environment is the environment name this environment relates to
	// +required
	Environment string `json:"environment"`

	// Type is the type of relationship: "After" or "Parallel"
	// +required
	// +kubebuilder:validation:Enum=After;Parallel
	Type RelationshipType `json:"type"`
}

// BackendConfig contains backend-specific configuration
type BackendConfig struct {
	// Type specifies the backend to use (e.g., "github")
	// +required
	// +kubebuilder:validation:Enum=github
	Type string `json:"type"`

	// Project is the project identifier (backend-specific format, e.g., "owner/repo" for GitHub)
	// +required
	Project string `json:"project"`

	// Secret is the name of the Kubernetes Secret containing the backend authentication token
	// +optional
	Secret string `json:"secret,omitempty"`
}

// EnvironmentSpec defines the desired state of Environment
type EnvironmentSpec struct {

	// RolloutRef is a reference to the Rollout that this Environment manages
	// +required
	RolloutRef corev1.LocalObjectReference `json:"rolloutRef"`

	// Name is the name of the GitHub deployment (the "kuberik" prefix will be automatically added for GitHub backend if not already present)
	// +required
	Name string `json:"name"`

	// Environment is the environment name (e.g., "production", "staging")
	// +optional
	Environment string `json:"environment,omitempty"`

	// Relationship defines how this environment relates to another environment
	// +optional
	Relationship *EnvironmentRelationship `json:"relationship,omitempty"`

	// Backend contains backend-specific configuration
	// +required
	Backend BackendConfig `json:"backend"`

	// RequeueInterval specifies how often the controller should reconcile this Environment
	// If not specified, defaults to 1 minute. Must be a valid duration string (e.g., "1m", "30s", "5m").
	// +optional
	// +kubebuilder:validation:Pattern=^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$
	RequeueInterval string `json:"requeueInterval,omitempty"`
}

// EnvironmentStatusEntry represents a deployment status for a specific version in an environment.
type EnvironmentStatusEntry struct {
	// Environment is the environment name (e.g., "production", "staging")
	// +required
	Environment string `json:"environment"`

	// DeploymentHistoryEntry embeds the entire deployment history entry from the Rollout
	kuberikrolloutv1alpha1.DeploymentHistoryEntry `json:",inline"`
}

// EnvironmentInfo represents information about an environment's deployment.
type EnvironmentInfo struct {
	// Environment is the environment name
	// +required
	Environment string `json:"environment"`

	// EnvironmentURL is the URL of the actual deployed environment (e.g., dashboard URL)
	// +optional
	EnvironmentURL string `json:"environmentUrl,omitempty"`

	// Relationship defines how this environment relates to another environment
	// +optional
	Relationship *EnvironmentRelationship `json:"relationship,omitempty"`
}

// EnvironmentStatus defines the observed state of Environment.
type EnvironmentStatus struct {
	// DeploymentID is the ID of the deployment (backend-specific)
	// +optional
	DeploymentID *int64 `json:"deploymentId,omitempty"`

	// DeploymentURL is the URL of the deployment (backend-specific)
	// +optional
	DeploymentURL string `json:"deploymentUrl,omitempty"`

	// LastStatusChangeTime is the last time the status was updated (only updated when status changes)
	// +optional
	LastStatusChangeTime *metav1.Time `json:"lastStatusChangeTime,omitempty"`

	// CurrentVersion is the current version being deployed
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// RolloutGateRef is a reference to the RolloutGate that was created/updated
	// +optional
	RolloutGateRef *corev1.LocalObjectReference `json:"rolloutGateRef,omitempty"`

	// DeploymentStatuses tracks deployment statuses per version and environment.
	// Only versions that are relevant based on environment relationships are tracked.
	// +listType=atomic
	// +optional
	DeploymentStatuses []EnvironmentStatusEntry `json:"deploymentStatuses,omitempty"`

	// EnvironmentInfos tracks deployment information for each environment.
	// Each environment has environment URL and relationships (not per version).
	// +listType=atomic
	// +optional
	EnvironmentInfos []EnvironmentInfo `json:"environmentInfos,omitempty"`

	// conditions represent the current state of the Environment resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Environment is the Schema for the environments API
type Environment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Environment
	// +required
	Spec EnvironmentSpec `json:"spec"`

	// status defines the observed state of Environment
	// +optional
	Status EnvironmentStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Environment{}, &EnvironmentList{})
}

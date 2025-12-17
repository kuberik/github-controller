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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DeploymentRelationship defines a relationship to another environment
type DeploymentRelationship struct {
	// Environment is the environment name this deployment relates to
	// +required
	Environment string `json:"environment"`

	// Type is the type of relationship: "after" or "togetherWith"
	// +required
	// +kubebuilder:validation:Enum=after;togetherWith
	Type string `json:"type"`
}

// DeploymentSpec defines the desired state of Deployment
type DeploymentSpec struct {
	// Backend specifies the deployment backend to use (e.g., "github")
	// +required
	// +kubebuilder:validation:Enum=github
	Backend string `json:"backend"`

	// RolloutRef is a reference to the Rollout that this Deployment manages
	// +required
	RolloutRef corev1.LocalObjectReference `json:"rolloutRef"`

	// Repository is the repository identifier (e.g., "owner/repo" for GitHub)
	// +required
	Repository string `json:"repository"`

	// DeploymentName is the name of the deployment
	// +required
	DeploymentName string `json:"deploymentName"`

	// Environment is the deployment environment (e.g., "production", "staging")
	// +optional
	Environment string `json:"environment,omitempty"`

	// Relationship defines how this deployment relates to another environment
	// +optional
	Relationship *DeploymentRelationship `json:"relationship,omitempty"`

	// RequiredContexts is a list of required status check contexts (backend-specific)
	// +optional
	RequiredContexts []string `json:"requiredContexts,omitempty"`

	// TokenSecret is the name of the Kubernetes Secret containing the backend token
	// +optional
	TokenSecret string `json:"tokenSecret,omitempty"`

	// RequeueInterval specifies how often the controller should reconcile this Deployment
	// If not specified, defaults to 1 minute. Must be a valid duration string (e.g., "1m", "30s", "5m").
	// +optional
	// +kubebuilder:validation:Pattern=^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$
	RequeueInterval string `json:"requeueInterval,omitempty"`
}

// DeploymentStatusEntry represents a deployment status for a specific version in an environment.
type DeploymentStatusEntry struct {
	// Environment is the deployment environment (e.g., "production", "staging")
	// +required
	Environment string `json:"environment"`

	// Version is the version/revision being tracked
	// +required
	Version string `json:"version"`

	// Status is the deployment status state (e.g., "success", "failure", "in_progress", "pending", "inactive")
	// +required
	Status string `json:"status"`

	// DeploymentID is the deployment ID (backend-specific)
	// +optional
	DeploymentID *int64 `json:"deploymentId,omitempty"`

	// DeploymentURL is the URL of the deployment (backend-specific)
	// +optional
	DeploymentURL string `json:"deploymentUrl,omitempty"`
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
	Relationship *DeploymentRelationship `json:"relationship,omitempty"`
}

// DeploymentStatus defines the observed state of Deployment.
type DeploymentStatus struct {
	// DeploymentID is the ID of the deployment (backend-specific)
	// +optional
	DeploymentID *int64 `json:"deploymentId,omitempty"`

	// DeploymentURL is the URL of the deployment (backend-specific)
	// +optional
	DeploymentURL string `json:"deploymentUrl,omitempty"`

	// LastSyncTime is the last time the deployment was synchronized
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

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
	DeploymentStatuses []DeploymentStatusEntry `json:"deploymentStatuses,omitempty"`

	// EnvironmentInfos tracks deployment information for each environment.
	// Each environment has environment URL and relationships (not per version).
	// +listType=atomic
	// +optional
	EnvironmentInfos []EnvironmentInfo `json:"environmentInfos,omitempty"`

	// conditions represent the current state of the Deployment resource.
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

// Deployment is the Schema for the deployments API
type Deployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Deployment
	// +required
	Spec DeploymentSpec `json:"spec"`

	// status defines the observed state of Deployment
	// +optional
	Status DeploymentStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DeploymentList contains a list of Deployment
type DeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Deployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Deployment{}, &DeploymentList{})
}

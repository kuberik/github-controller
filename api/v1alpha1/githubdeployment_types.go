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

// GitHubDeploymentSpec defines the desired state of GitHubDeployment
type GitHubDeploymentSpec struct {
	// RolloutRef is a reference to the Rollout that this GitHubDeployment manages
	// +required
	RolloutRef corev1.LocalObjectReference `json:"rolloutRef"`

	// Repository is the GitHub repository in format "owner/repo"
	// +required
	Repository string `json:"repository"`

	// DeploymentName is the name of the GitHub deployment
	// +required
	DeploymentName string `json:"deploymentName"`

	// Environment is the GitHub deployment environment (e.g., "production", "staging")
	// +optional
	Environment string `json:"environment,omitempty"`

	// Dependencies is a list of deployment names that this deployment depends on
	// +optional
	Dependencies []string `json:"dependencies,omitempty"`

	// RequiredContexts is a list of required status check contexts
	// +optional
	RequiredContexts []string `json:"requiredContexts,omitempty"`

	// GitHubTokenSecret is the name of the Kubernetes Secret containing the GitHub token
	// +optional
	GitHubTokenSecret string `json:"githubTokenSecret,omitempty"`
}

// GitHubDeploymentStatus defines the observed state of GitHubDeployment.
type GitHubDeploymentStatus struct {
	// GitHubDeploymentID is the ID of the GitHub deployment
	// +optional
	GitHubDeploymentID *int64 `json:"githubDeploymentId,omitempty"`

	// GitHubDeploymentURL is the URL of the GitHub deployment
	// +optional
	GitHubDeploymentURL string `json:"githubDeploymentUrl,omitempty"`

	// LastSyncTime is the last time the GitHub deployment was synchronized
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// CurrentVersion is the current version being deployed
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// RolloutGateRef is a reference to the RolloutGate that was created/updated
	// +optional
	RolloutGateRef *corev1.LocalObjectReference `json:"rolloutGateRef,omitempty"`

	// conditions represent the current state of the GitHubDeployment resource.
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

// GitHubDeployment is the Schema for the githubdeployments API
type GitHubDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of GitHubDeployment
	// +required
	Spec GitHubDeploymentSpec `json:"spec"`

	// status defines the observed state of GitHubDeployment
	// +optional
	Status GitHubDeploymentStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// GitHubDeploymentList contains a list of GitHubDeployment
type GitHubDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitHubDeployment{}, &GitHubDeploymentList{})
}

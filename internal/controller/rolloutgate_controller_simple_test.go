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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

func TestIsGitHubGate(t *testing.T) {
	reconciler := &RolloutGateReconciler{}

	tests := []struct {
		name        string
		rolloutGate *kuberikrolloutv1alpha1.RolloutGate
		expected    bool
	}{
		{
			name: "GitHub gate class annotation",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationKeyGateClass: AnnotationValueGitHubGateClass,
					},
				},
			},
			expected: true,
		},
		{
			name:        "No GitHub configuration",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{},
			expected:    false,
		},
		{
			name: "Wrong gate class annotation",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationKeyGateClass: "other",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reconciler.isGitHubGate(tt.rolloutGate)
			if result != tt.expected {
				t.Errorf("isGitHubGate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestSlicesEqual(t *testing.T) {
	reconciler := &RolloutGateReconciler{}

	tests := []struct {
		name     string
		a        *[]string
		b        *[]string
		expected bool
	}{
		{
			name:     "Equal slices",
			a:        &[]string{"a", "b", "c"},
			b:        &[]string{"a", "b", "c"},
			expected: true,
		},
		{
			name:     "Different slices",
			a:        &[]string{"a", "b", "c"},
			b:        &[]string{"a", "b", "d"},
			expected: false,
		},
		{
			name:     "Nil slices",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "One nil slice",
			a:        nil,
			b:        &[]string{"a", "b", "c"},
			expected: false,
		},
		{
			name:     "Different lengths",
			a:        &[]string{"a", "b"},
			b:        &[]string{"a", "b", "c"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := reconciler.slicesEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("slicesEqual() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

func TestGetGitHubConfig(t *testing.T) {
	reconciler := &RolloutGateReconciler{}

	tests := []struct {
		name        string
		rolloutGate *kuberikrolloutv1alpha1.RolloutGate
		expected    *GitHubConfig
		expectError bool
	}{
		{
			name: "Valid GitHub configuration",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationKeyGitHubRepo:       "test/repo",
						AnnotationKeyDeploymentName:   "test-deployment",
						AnnotationKeyEnvironment:      "production",
						AnnotationKeyRef:              "main",
						AnnotationKeyDescription:      "Test deployment",
						AnnotationKeyAutoMerge:        "true",
						AnnotationKeyDependencies:     "dep1,dep2",
						AnnotationKeyRequiredContexts: "ci,security",
					},
				},
			},
			expected: &GitHubConfig{
				Repository:       "test/repo",
				DeploymentName:   "test-deployment",
				Environment:      "production",
				Ref:              "main",
				Description:      "Test deployment",
				AutoMerge:        true,
				Dependencies:     []string{"dep1", "dep2"},
				RequiredContexts: []string{"ci", "security"},
			},
			expectError: false,
		},
		{
			name: "Missing required annotations",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationKeyGitHubRepo: "test/repo",
						// Missing deployment name
					},
				},
			},
			expected:    nil,
			expectError: true,
		},
		{
			name:        "No annotations",
			rolloutGate: &kuberikrolloutv1alpha1.RolloutGate{},
			expected:    nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := reconciler.getGitHubConfig(nil, tt.rolloutGate)

			if tt.expectError {
				if err == nil {
					t.Errorf("getGitHubConfig() expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("getGitHubConfig() unexpected error: %v", err)
				return
			}

			if result.Repository != tt.expected.Repository {
				t.Errorf("getGitHubConfig() Repository = %v, expected %v", result.Repository, tt.expected.Repository)
			}
			if result.DeploymentName != tt.expected.DeploymentName {
				t.Errorf("getGitHubConfig() DeploymentName = %v, expected %v", result.DeploymentName, tt.expected.DeploymentName)
			}
			if result.Environment != tt.expected.Environment {
				t.Errorf("getGitHubConfig() Environment = %v, expected %v", result.Environment, tt.expected.Environment)
			}
			if result.AutoMerge != tt.expected.AutoMerge {
				t.Errorf("getGitHubConfig() AutoMerge = %v, expected %v", result.AutoMerge, tt.expected.AutoMerge)
			}
		})
	}
}

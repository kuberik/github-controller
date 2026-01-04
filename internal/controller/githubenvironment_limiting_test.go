package controller

import (
	"testing"

	kuberikv1alpha1 "github.com/kuberik/environment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
	k8sptr "k8s.io/utils/ptr"
)

func TestBuildEnvironmentInfos_LimitsHistoryForIndirectConnections(t *testing.T) {
	// Setup
	r := &GitHubEnvironmentReconciler{}

	// Define environments
	// env-a (current)
	// env-b (depends on env-a) -> Direct
	// env-c (depends on env-b) -> Indirect

	currentEnvName := "env-a"

	envA := &kuberikv1alpha1.Environment{
		Spec: kuberikv1alpha1.EnvironmentSpec{
			Environment: currentEnvName,
		},
		Status: kuberikv1alpha1.EnvironmentStatus{},
	}

	// Create history entries
	createHistory := func(cnt int) []kuberikrolloutv1alpha1.DeploymentHistoryEntry {
		h := make([]kuberikrolloutv1alpha1.DeploymentHistoryEntry, cnt)
		for i := 0; i < cnt; i++ {
			revision := "rev"
			h[i] = kuberikrolloutv1alpha1.DeploymentHistoryEntry{
				ID: k8sptr.To(int64(cnt - i)), // 3, 2, 1
				Version: kuberikrolloutv1alpha1.VersionInfo{
					Revision: &revision,
				},
			}
		}
		return h
	}

	relationships := map[string]*kuberikv1alpha1.EnvironmentRelationship{
		"env-b": {Environment: "env-a"},
		"env-c": {Environment: "env-b"},
	}

	graphData := &relationshipGraphData{
		environmentInfos: map[string]environmentInfo{
			"env-a": {Relationship: nil},
			"env-b": {Relationship: relationships["env-b"]},
			"env-c": {Relationship: relationships["env-c"]},
		},
		envHistory: map[string][]kuberikrolloutv1alpha1.DeploymentHistoryEntry{
			"env-a": createHistory(3),
			"env-b": createHistory(3),
			"env-c": createHistory(3),
		},
	}

	// Execute
	infos := r.buildEnvironmentInfos(envA, graphData)

	// Assert
	infoMap := make(map[string]kuberikv1alpha1.EnvironmentInfo)
	for _, info := range infos {
		infoMap[info.Environment] = info
	}

	// Helper to assert history length
	assertHistoryLen := func(envName string, expected int) {
		info, ok := infoMap[envName]
		if !ok {
			t.Errorf("Expected environment %s to be present", envName)
			return
		}
		if len(info.History) != expected {
			t.Errorf("Environment %s: expected history length %d, got %d", envName, expected, len(info.History))
		}
	}

	// env-a: Current env, should have full history
	assertHistoryLen("env-a", 3)

	// env-b: Directly connected (depends on current), should have full history
	assertHistoryLen("env-b", 3)

	// env-c: Indirectly connected, should have LIMITED history (1)
	assertHistoryLen("env-c", 1)
}

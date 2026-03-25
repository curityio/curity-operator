package controller

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// setCondition sets a condition on the given conditions slice.
// It always sets ObservedGeneration to the given generation.
func setCondition(conditions *[]metav1.Condition, condType string, status metav1.ConditionStatus, reason, message string, generation int64) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// computeNodeConditions derives status conditions from a Deployment's status.
// Returns the conditions slice and replica counts for the node status.
func computeNodeConditions(deploy *appsv1.Deployment, generation int64) ([]metav1.Condition, nodeReplicaStatus) {
	var conditions []metav1.Condition
	var rs nodeReplicaStatus

	if deploy == nil {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, "DeploymentNotFound", "Deployment does not exist yet", generation)
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse, "DeploymentNotFound", "Deployment does not exist yet", generation)
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue, "DeploymentNotFound", "Waiting for Deployment to be created", generation)
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, "DeploymentNotFound", "Deployment does not exist yet", generation)
		return conditions, rs
	}

	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	rs = nodeReplicaStatus{
		Replicas:            desired,
		UpdatedReplicas:     deploy.Status.UpdatedReplicas,
		ReadyReplicas:       deploy.Status.ReadyReplicas,
		AvailableReplicas:   deploy.Status.AvailableReplicas,
		UnavailableReplicas: deploy.Status.UnavailableReplicas,
	}

	// Ready: true when all desired replicas are ready
	if deploy.Status.ReadyReplicas >= desired && desired > 0 {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionTrue, "AllReplicasReady", "All replicas are ready", generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, "ReplicasNotReady",
			"Not all replicas are ready", generation)
	}

	// Available: true when the minimum number of replicas are running stably.
	// Mirrors Kubernetes Deployment Available condition semantics —
	// availableReplicas counts pods that have been ready for minReadySeconds.
	if deploy.Status.AvailableReplicas >= desired && desired > 0 {
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionTrue, "MinimumReplicasAvailable", "Deployment has minimum availability", generation)
	} else if deploy.Status.AvailableReplicas > 0 {
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse, "InsufficientReplicas",
			fmt.Sprintf("%d/%d replicas available", deploy.Status.AvailableReplicas, desired), generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse, "NoReplicaAvailable", "No replicas are available", generation)
	}

	// Progressing: true when rollout is not yet settled.
	// A rollout is in progress when either:
	// - updated replicas < desired (new spec not fully rolled out)
	// - unavailable replicas > 0 (pods crashing or not yet ready)
	if deploy.Status.UpdatedReplicas < desired {
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue, "RolloutInProgress", "Deployment rollout is in progress", generation)
	} else if deploy.Status.UnavailableReplicas > 0 {
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue, "ReplicaUnavailable",
			fmt.Sprintf("%d replica(s) unavailable", deploy.Status.UnavailableReplicas), generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, "RolloutComplete", "Deployment rollout is complete", generation)
	}

	// Degraded: true when not all replicas are ready but some are running
	// (partial failure — serving traffic but below desired capacity)
	if deploy.Status.ReadyReplicas < desired && deploy.Status.AvailableReplicas > 0 {
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, "PartiallyAvailable",
			fmt.Sprintf("Only %d/%d replicas ready, but serving traffic", deploy.Status.ReadyReplicas, desired), generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, "Healthy", "All replicas are healthy", generation)
	}

	return conditions, rs
}

// nodeReplicaStatus holds replica counts derived from a Deployment.
type nodeReplicaStatus struct {
	Replicas            int32
	UpdatedReplicas     int32
	ReadyReplicas       int32
	AvailableReplicas   int32
	UnavailableReplicas int32
}

// computeClusterConditions derives cluster-level conditions from child nodes.
func computeClusterConditions(nodes []v1alpha1.IdentityServerNode, generation int64) []metav1.Condition {
	var conditions []metav1.Condition

	if len(nodes) == 0 {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, "NoNodes", "No nodes reference this cluster", generation)
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse, "NoNodes", "No nodes reference this cluster", generation)
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue, "WaitingForNodes", "Waiting for nodes to be created", generation)
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, "NoNodes", "No nodes reference this cluster", generation)
		return conditions
	}

	allReady := true
	allAvailable := true
	anyDegraded := false
	adminCount := 0

	for i := range nodes {
		node := &nodes[i]
		if node.Spec.Type == v1alpha1.NodeTypeAdmin {
			adminCount++
		}

		readyCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
		if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
			allReady = false
		}

		availCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionAvailable)
		if availCond == nil || availCond.Status != metav1.ConditionTrue {
			allAvailable = false
		}

		degradedCond := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionDegraded)
		if degradedCond != nil && degradedCond.Status == metav1.ConditionTrue {
			anyDegraded = true
		}
	}

	// Ready: all nodes have all desired replicas running
	if allReady {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionTrue, "AllNodesReady", "All nodes are ready", generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionReady, metav1.ConditionFalse, "NodesNotReady", "Not all nodes are ready", generation)
	}

	// Available: all nodes meet minimum stable availability
	// (mirrors K8s Deployment Available = minReadySeconds satisfied)
	if allAvailable {
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionTrue, "AllNodesAvailable", "All nodes have minimum availability", generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse, "NodesUnavailable", "Not all nodes have minimum availability", generation)
	}

	// Progressing
	if !allReady {
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue, "NodesProgressing", "Some nodes are not yet ready", generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse, "Stable", "All nodes are ready", generation)
	}

	// Degraded
	if adminCount > 1 {
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, "MultipleAdminNodes",
			"More than one admin node exists for this cluster", generation)
	} else if anyDegraded {
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue, "NodeDegraded", "One or more nodes are degraded", generation)
	} else {
		setCondition(&conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse, "Healthy", "Cluster is healthy", generation)
	}

	return conditions
}

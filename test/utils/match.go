package utils

import (
	"fmt"
	"path/filepath"
	"strings"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	"github.com/gkampitakis/go-snaps/match"
	"github.com/gkampitakis/go-snaps/snaps"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/dsl/core"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var excludeFields = []string{
	"$.metadata.uid",
	"$.metadata.resourceVersion",
	"$.metadata.generation",
	"$.metadata.creationTimestamp",
	"$.metadata.annotations",
	"$.metadata.managedFields",
	"$.metadata.ownerReferences",
	"$.spec.selector",
	"$.spec.template.metadata",
	"$.spec.clusterIP",
	"$.spec.clusterIPs",
	"$.status",
}

var excludeJobFields = append(excludeFields, []string{
	"$.metadata.name",
}...)

var excludeCRDFields = []string{
	"$.metadata.uid",
	"$.metadata.resourceVersion",
	"$.metadata.generation",
	"$.metadata.creationTimestamp",
	"$.metadata.annotations",
	"$.metadata.managedFields",
	"$.metadata.ownerReferences",
	"$.status.observedGeneration",
	// Replica counts vary by environment (Kind vs AKS) and timing
	"$.status.updatedReplicas",
	"$.status.readyReplicas",
	"$.status.availableReplicas",
	"$.status.unavailableReplicas",
	"$.status.readyNodes",
}

var excludeFieldMap = map[string][]string{
	"job":                   excludeJobFields,
	"identityservercluster": excludeCRDFields,
	"identityservernode":    excludeCRDFields,
}

// MatchYAMLResource takes a snapshot of the resource and compares it.
// The kind is auto-detected from the object type.
func MatchYAMLResource(resource interface{}, snapshotName ...string) {
	kind := strings.ToLower(GetKind(resource))
	currentSpec := ginkgo.CurrentSpecReport()
	name := strings.Join(snapshotName, "_")
	if name == "" {
		name = currentSpec.LeafNodeText
	}

	name = fmt.Sprintf("[%s] %s", kind, name)
	exclude, ok := excludeFieldMap[kind]
	if !ok {
		exclude = excludeFields
	}

	snaps.WithConfig(
		snaps.Dir(fmt.Sprintf("__snapshots__/%s/%s", filepath.Base(currentSpec.FileName()), currentSpec.LeafNodeText)),
		snaps.Filename(name),
		snaps.Ext(".yaml"),
	).MatchYAML(
		core.GinkgoT(),
		resource,
		match.Any(exclude...).ErrOnMissingPath(false),
	)
}

// MatchCRDResource takes a snapshot of a CRD resource with status included.
// It deep-copies the resource and zeroes volatile condition fields
// (LastTransitionTime, ObservedGeneration) before snapshotting.
func MatchCRDResource(resource interface{}, snapshotName ...string) {
	var sanitized interface{}

	switch r := resource.(type) {
	case *v1alpha1.IdentityServerCluster:
		c := r.DeepCopy()
		sanitizeConditions(c.Status.Conditions)
		sanitized = c
	case *v1alpha1.IdentityServerNode:
		c := r.DeepCopy()
		sanitizeConditions(c.Status.Conditions)
		sanitized = c
	default:
		sanitized = resource
	}

	MatchYAMLResource(sanitized, snapshotName...)
}

// sanitizeConditions zeroes volatile fields on each condition
// so snapshots remain deterministic across test runs and environments.
// Reason and Message vary by Deployment controller timing (e.g.,
// ReplicaUnavailable vs RolloutInProgress), so only Type and Status
// are kept for cross-environment snapshot compatibility.
func sanitizeConditions(conditions []metav1.Condition) {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Time{}
		conditions[i].ObservedGeneration = 0
		conditions[i].Reason = ""
		conditions[i].Message = ""
	}
}

// MatchResource takes a snapshot with an explicit kind parameter.
func MatchResource(resource interface{}, kind string, snapshotName ...string) {
	currentSpec := ginkgo.CurrentSpecReport()
	name := strings.Join(snapshotName, "_")
	if name == "" {
		name = currentSpec.LeafNodeText
	}

	name = fmt.Sprintf("[%s] %s", kind, name)
	exclude, ok := excludeFieldMap[kind]
	if !ok {
		exclude = excludeFields
	}

	snaps.WithConfig(
		snaps.Dir(fmt.Sprintf("__snapshots__/%s/%s", filepath.Base(currentSpec.FileName()), currentSpec.LeafNodeText)),
		snaps.Filename(name),
		snaps.Ext(".yaml"),
	).MatchYAML(
		core.GinkgoT(),
		resource,
		match.Any(exclude...).ErrOnMissingPath(false),
	)
}

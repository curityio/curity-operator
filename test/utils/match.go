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
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	policyv1 "k8s.io/api/policy/v1"
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
	// Replica/node counts vary by environment (Kind vs AKS) and timing
	"$.status.updatedReplicas",
	"$.status.readyReplicas",
	"$.status.availableReplicas",
	"$.status.unavailableReplicas",
	"$.status.readyNodes",
	"$.status.nodeCount",
}

// excludeServiceMonitorFields strips only volatile metadata — selector,
// endpoints, namespaceSelector, and labels are deterministic and worth
// snapshotting (the default excludeFields drops spec.selector, which is the
// crux of a ServiceMonitor).
var excludeServiceMonitorFields = []string{
	"$.metadata.uid",
	"$.metadata.resourceVersion",
	"$.metadata.generation",
	"$.metadata.creationTimestamp",
	"$.metadata.annotations",
	"$.metadata.managedFields",
	"$.metadata.ownerReferences",
}

var excludeFieldMap = map[string][]string{
	"job":                     excludeJobFields,
	"horizontalpodautoscaler": excludeFields,
	"poddisruptionbudget":     excludeFields,
	"identityservercluster":   excludeCRDFields,
	"identityservernode":      excludeCRDFields,
	"servicemonitor":          excludeServiceMonitorFields,
}

// MatchYAMLResource takes a snapshot of the resource and compares it.
// The kind is auto-detected from the object type.
func MatchYAMLResource(resource interface{}, snapshotName ...string) {
	resource = sanitizeVolatileFields(resource)
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
// It deep-copies the resource and zeroes volatile fields before snapshotting
// so that snapshots remain deterministic across environments (Kind vs AKS).
func MatchCRDResource(resource interface{}, snapshotName ...string) {
	var sanitized interface{}

	switch r := resource.(type) {
	case *v1alpha1.IdentityServerCluster:
		c := r.DeepCopy()
		sanitizeConditions(c.Status.Conditions)
		c.Annotations = nil
		c.Status.NodeCount = 0
		c.Status.ReadyNodes = 0
		// Populated only where the ServiceMonitor CRD is installed — varies by
		// environment (Kind vs AKS), so zero it for deterministic snapshots.
		c.Status.ServiceMonitorName = ""
		sanitized = c
	case *v1alpha1.IdentityServerNode:
		c := r.DeepCopy()
		sanitizeConditions(c.Status.Conditions)
		c.Annotations = nil
		c.Status.UpdatedReplicas = 0
		c.Status.ReadyReplicas = 0
		c.Status.AvailableReplicas = 0
		c.Status.UnavailableReplicas = 0
		sanitized = c
	default:
		sanitized = resource
	}

	MatchYAMLResource(sanitized, snapshotName...)
}

// sanitizeConditions zeroes ALL volatile fields on each condition so snapshots
// remain deterministic across test runs and environments (Kind vs AKS).
// Only the condition Type is preserved. Status, Reason, and Message all vary
// by timing and environment — e.g., Kind may show Ready=True/Healthy while
// AKS shows Ready=False/NodeDegraded for the same snapshot point.
// Conditions are verified by inline Eventually/Expect assertions instead.
func sanitizeConditions(conditions []metav1.Condition) {
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Time{}
		conditions[i].ObservedGeneration = 0
		conditions[i].Status = ""
		conditions[i].Reason = ""
		conditions[i].Message = ""
	}
}

// sanitizeVolatileFields strips fields from Kubernetes resources that are
// set asynchronously by controllers and may or may not be present depending
// on timing. match.Any exclusions only handle value variation, not
// presence/absence variation caused by omitempty on zero-valued fields.
func sanitizeVolatileFields(resource interface{}) interface{} {
	switch r := resource.(type) {
	case *appsv1.Deployment:
		d := r.DeepCopy()
		d.Annotations = nil
		d.Status = appsv1.DeploymentStatus{}
		return d
	case *autoscalingv2.HorizontalPodAutoscaler:
		h := r.DeepCopy()
		h.Annotations = nil
		h.Status = autoscalingv2.HorizontalPodAutoscalerStatus{}
		return h
	case *batchv1.Job:
		j := r.DeepCopy()
		j.Status = batchv1.JobStatus{}
		return j
	case *policyv1.PodDisruptionBudget:
		p := r.DeepCopy()
		p.Annotations = nil
		p.Status = policyv1.PodDisruptionBudgetStatus{}
		return p
	default:
		return resource
	}
}

// MatchResource takes a snapshot with an explicit kind parameter.
func MatchResource(resource interface{}, kind string, snapshotName ...string) {
	resource = sanitizeVolatileFields(resource)
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

package utils

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gkampitakis/go-snaps/match"
	"github.com/gkampitakis/go-snaps/snaps"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/dsl/core"
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
	"$.status.conditions[*].lastTransitionTime",
	"$.status.conditions[*].lastProbeTime",
	"$.status.conditions[*].lastUpdateTime",
}

var excludeJobFields = append(excludeFields, []string{
	"$.metadata.name",
}...)

var excludeFieldMap = map[string][]string{
	"job": excludeJobFields,
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

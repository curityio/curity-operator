package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Labels and annotations for config discovery.
const (
	LabelManagedConfig     = "curity.io/managed"
	AnnotationConfigType   = "curity.io/config-type"
	AnnotationClusterScope = "curity.io/cluster"
)

// Event reasons related to cluster-scope annotation handling.
const (
	EventReasonEmptyClusterScope     = "EmptyClusterScope"
	EventReasonUnknownClusterInScope = "UnknownClusterInScope"
)

// Config type values matching the README.
const (
	ConfigTypeBase    = "base"
	ConfigTypeLicense = "license"
)

// Mount paths per config type.
const (
	MountPathBase    = "/opt/idsvr/etc/init/"
	MountPathLicense = "/opt/idsvr/etc/init/license/"
)

// Volume name prefixes to avoid collisions with cluster-config.
const (
	volumePrefixConfigMap = "cfg-cm-"
	volumePrefixSecret    = "cfg-secret-"
	maxVolumeNameLength   = 63
)

// ErrUnknownConfigType is returned when a managed resource has an unrecognized
// curity.io/config-type annotation value.
var ErrUnknownConfigType = errors.New("unknown config type")

// DiscoveredConfigResource represents a ConfigMap or Secret found via
// label-based discovery (curity.io/managed=true).
type DiscoveredConfigResource struct {
	Name       string
	IsSecret   bool
	ConfigType string
	Data       map[string][]byte
}

// SkippedResource records a managed resource that was excluded from a
// discovery pass because its annotations are invalid. Surfaced so callers
// can emit a per-resource Warning Event without blocking reconciliation of
// the remaining valid resources — a single bad neighbor must not cascade
// into the entire managed-config feature going dark.
type SkippedResource struct {
	Name   string
	Kind   string // "ConfigMap" or "Secret"
	Reason string // stable, human-readable; used for EventRecorder dedup
}

// parseClusterScope returns the set of cluster names the curity.io/cluster
// annotation value designates. Whitespace is trimmed, empty entries are
// dropped, and duplicates collapse. A value that yields no usable names
// (empty string, whitespace, or all-empty entries like ",,") returns
// (nil, true); the caller decides how to interpret that.
func parseClusterScope(val string) (names map[string]struct{}, empty bool) {
	parts := strings.Split(val, ",")
	names = make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		names[p] = struct{}{}
	}
	if len(names) == 0 {
		return nil, true
	}
	return names, false
}

// appliesToCluster reports whether a resource's annotations designate the
// given cluster as in-scope. Absent annotation → applies to all clusters.
// Annotation present but empty (or otherwise yielding no names) → applies
// to no cluster; this is treated as a user mistake and surfaced separately
// via scanConfigScopeIssues rather than silently mounting everywhere.
func appliesToCluster(annotations map[string]string, clusterName string) bool {
	val, ok := annotations[AnnotationClusterScope]
	if !ok {
		return true
	}
	names, empty := parseClusterScope(val)
	if empty {
		return false
	}
	_, ok = names[clusterName]
	return ok
}

// discoverConfigResources lists ConfigMaps and Secrets labeled
// curity.io/managed=true in the given namespace, filtered by the
// curity.io/cluster annotation to those applying to clusterName.
// Operator-internal cluster-config secrets are filtered out.
//
// Returns (configs, skipped, error). Resources with an invalid
// curity.io/config-type annotation are reported via `skipped` and omitted
// from `configs` — one bad annotation must not block validation of the
// remaining valid resources. Hard errors (e.g. List failures) still go in
// `error`. Results are sorted by name for deterministic ordering.
func discoverConfigResources(ctx context.Context, c client.Client, namespace, clusterName string) ([]DiscoveredConfigResource, []SkippedResource, error) {
	managedLabel := client.MatchingLabels{LabelManagedConfig: "true"}

	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, nil, fmt.Errorf("listing managed ConfigMaps: %w", err)
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, nil, fmt.Errorf("listing managed Secrets: %w", err)
	}

	result := make([]DiscoveredConfigResource, 0, len(configMaps.Items)+len(secrets.Items))
	var skipped []SkippedResource

	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		if !appliesToCluster(cm.Annotations, clusterName) {
			continue
		}
		configType, err := resolveConfigType(cm.Annotations)
		if err != nil {
			skipped = append(skipped, SkippedResource{
				Name:   cm.Name,
				Kind:   "ConfigMap",
				Reason: err.Error(),
			})
			continue
		}
		data := make(map[string][]byte, len(cm.Data))
		for k, v := range cm.Data {
			data[k] = []byte(v)
		}
		result = append(result, DiscoveredConfigResource{
			Name:       cm.Name,
			IsSecret:   false,
			ConfigType: configType,
			Data:       data,
		})
	}

	for i := range secrets.Items {
		s := &secrets.Items[i]
		// Filter out operator-internal cluster-config secrets.
		if s.Labels["curity.io/component"] == "cluster-config" {
			continue
		}
		if !appliesToCluster(s.Annotations, clusterName) {
			continue
		}
		configType, err := resolveConfigType(s.Annotations)
		if err != nil {
			skipped = append(skipped, SkippedResource{
				Name:   s.Name,
				Kind:   "Secret",
				Reason: err.Error(),
			})
			continue
		}
		result = append(result, DiscoveredConfigResource{
			Name:       s.Name,
			IsSecret:   true,
			ConfigType: configType,
			Data:       s.Data,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	sort.Slice(skipped, func(i, j int) bool {
		return skipped[i].Name < skipped[j].Name
	})

	return result, skipped, nil
}

// defaultConfigTypeAnnotations writes the curity.io/config-type annotation on
// managed ConfigMaps/Secrets when absent, so users can see the effective type.
// Only resources applying to clusterName are considered, so two clusters in the
// same namespace do not race to default-annotate each other's resources.
// Only called from the cluster reconciler (which has update RBAC on configmaps/secrets).
func defaultConfigTypeAnnotations(ctx context.Context, c client.Client, namespace, clusterName string) error {
	managedLabel := client.MatchingLabels{LabelManagedConfig: "true"}

	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(namespace), managedLabel); err != nil {
		return fmt.Errorf("listing managed ConfigMaps: %w", err)
	}
	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		if !appliesToCluster(cm.Annotations, clusterName) {
			continue
		}
		if cm.Annotations[AnnotationConfigType] != "" {
			continue
		}
		if cm.Annotations == nil {
			cm.Annotations = make(map[string]string)
		}
		cm.Annotations[AnnotationConfigType] = ConfigTypeBase
		if err := c.Update(ctx, cm); err != nil {
			return fmt.Errorf("setting default config-type on configmap %q: %w", cm.Name, err)
		}
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace), managedLabel); err != nil {
		return fmt.Errorf("listing managed Secrets: %w", err)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Labels["curity.io/component"] == "cluster-config" {
			continue
		}
		if !appliesToCluster(s.Annotations, clusterName) {
			continue
		}
		if s.Annotations[AnnotationConfigType] != "" {
			continue
		}
		if s.Annotations == nil {
			s.Annotations = make(map[string]string)
		}
		s.Annotations[AnnotationConfigType] = ConfigTypeBase
		if err := c.Update(ctx, s); err != nil {
			return fmt.Errorf("setting default config-type on secret %q: %w", s.Name, err)
		}
	}

	return nil
}

// formatUnknownClusterMessage returns the byte-stable UnknownClusterInScope
// event message. The message is byte-exact to let K8s EventRecorder dedup
// repeated emissions — sorting happens inside formatNameList so callers
// can't silently break dedup by passing an unsorted slice.
func formatUnknownClusterMessage(missing, applied []string) string {
	return fmt.Sprintf(
		"curity.io/cluster references clusters not found in namespace: %s. Config applied to existing clusters %s. Missing clusters will apply if/when created.",
		formatNameList(missing), formatNameList(applied),
	)
}

// formatEmptyClusterScopeMessage returns the byte-stable EmptyClusterScope
// event message.
func formatEmptyClusterScopeMessage() string {
	return "curity.io/cluster annotation is empty; treating as applies-to-no-cluster. Remove the annotation to apply to all, or set a cluster list."
}

// formatNameList renders a slice as "[a, b, c]" with names in sorted order
// so the output is byte-stable. Sorts a copy — the caller's slice is not
// mutated.
func formatNameList(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)
	return "[" + strings.Join(sorted, ", ") + "]"
}

// ScopeIssue is a single problem observed when scanning managed resources
// for curity.io/cluster scope annotations.
type ScopeIssue struct {
	Kind    string // "ConfigMap" or "Secret"
	Name    string
	Empty   bool     // annotation was present but empty
	Unknown []string // cluster names in the annotation that don't exist
	Applied []string // cluster names in the annotation that do exist (companion to Unknown)
}

// scanConfigScopeIssues lists all managed resources in a namespace and returns
// the aggregate scope-annotation issues against the provided cluster-name set.
// Returns nil when there are no issues. Results are sorted for stable output.
func scanConfigScopeIssues(ctx context.Context, c client.Client, namespace string, existingClusters map[string]struct{}) ([]ScopeIssue, error) {
	managedLabel := client.MatchingLabels{LabelManagedConfig: "true"}

	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, fmt.Errorf("listing managed ConfigMaps: %w", err)
	}
	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, fmt.Errorf("listing managed Secrets: %w", err)
	}

	var issues []ScopeIssue
	eval := func(kind, name string, ann map[string]string) {
		raw, present := ann[AnnotationClusterScope]
		if !present {
			return
		}
		names, empty := parseClusterScope(raw)
		if empty {
			// Annotation present but yields no names (e.g. "", "  ", ",,").
			// Under applies-to-none semantics this mounts nowhere, which is
			// almost always a mistake — flag uniformly.
			issues = append(issues, ScopeIssue{Kind: kind, Name: name, Empty: true})
			return
		}
		var missing, applied []string
		for n := range names {
			if _, ok := existingClusters[n]; ok {
				applied = append(applied, n)
			} else {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			sort.Strings(applied)
			issues = append(issues, ScopeIssue{Kind: kind, Name: name, Unknown: missing, Applied: applied})
		}
	}
	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		eval("ConfigMap", cm.Name, cm.Annotations)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Labels["curity.io/component"] == "cluster-config" {
			continue
		}
		eval("Secret", s.Name, s.Annotations)
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return issues[i].Name < issues[j].Name
	})
	return issues, nil
}

// resolveConfigType reads the curity.io/config-type annotation and validates it.
// Empty or absent annotation defaults to "base". Unknown values return an error.
func resolveConfigType(annotations map[string]string) (string, error) {
	val := annotations[AnnotationConfigType]
	switch val {
	case "", ConfigTypeBase:
		return ConfigTypeBase, nil
	case ConfigTypeLicense:
		return ConfigTypeLicense, nil
	default:
		return "", fmt.Errorf("%w: %q (valid: %q, %q)", ErrUnknownConfigType, val, ConfigTypeBase, ConfigTypeLicense)
	}
}

// mountPathForConfigType returns the mount base path for the given config type.
func mountPathForConfigType(configType string) string {
	if configType == ConfigTypeLicense {
		return MountPathLicense
	}
	return MountPathBase
}

// shouldMountConfig determines whether a node should receive discovered configs.
// When an admin node exists, only admin gets configs (it distributes to runtimes
// via the Curity clustering protocol). Without an admin, all nodes get configs.
func shouldMountConfig(nodeType v1alpha1.NodeType, adminExists bool) bool {
	if !adminExists {
		return true
	}
	return nodeType == v1alpha1.NodeTypeAdmin
}

// computeConfigHash returns a deterministic SHA256 hex string over the sorted
// config names and their data. Returns empty string for nil or empty configs.
func computeConfigHash(configs []DiscoveredConfigResource) string {
	if len(configs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, cfg := range configs {
		h.Write([]byte(cfg.Name))
		h.Write([]byte{0})
		if cfg.IsSecret {
			h.Write([]byte("Secret"))
		} else {
			h.Write([]byte("ConfigMap"))
		}
		h.Write([]byte{0})
		h.Write([]byte(cfg.ConfigType))
		h.Write([]byte{0})
		keys := make([]string, 0, len(cfg.Data))
		for k := range cfg.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write(cfg.Data[k])
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// configVolumeName returns a Kubernetes-safe volume name for a discovered config.
// Uses "cfg-cm-" prefix for ConfigMaps and "cfg-secret-" for Secrets to avoid
// collisions between resources with the same name but different kinds.
// Truncates and appends a hash suffix if the name exceeds 63 characters.
func configVolumeName(isSecret bool, name string) string {
	prefix := volumePrefixConfigMap
	if isSecret {
		prefix = volumePrefixSecret
	}
	fullName := prefix + name
	if len(fullName) <= maxVolumeNameLength {
		return fullName
	}
	// Truncate and add a short hash for uniqueness.
	hash := sha256.Sum256([]byte(fullName))
	suffix := hex.EncodeToString(hash[:4])
	truncLen := maxVolumeNameLength - len(suffix) - 1 // -1 for the dash separator
	return fullName[:truncLen] + "-" + suffix
}

// mountFilename returns a unique filename for mounting a config data key.
// It prefixes the key with the resource kind and name to prevent mount path
// collisions when multiple resources contain the same data key.
// Uses "_" as separator because Kubernetes resource names cannot contain
// underscores (DNS subdomain rules), making the boundary unambiguous.
// Volume names use "-" (cfg-cm-{name}) because K8s requires DNS labels there;
// mount filenames use "_" (cm_{name}_{key}) because they're filesystem paths.
// ConfigMaps produce "cm_{name}_{key}", Secrets produce "secret_{name}_{key}".
// Note: no length truncation — assumes resource names and data keys are short
// enough that the result stays under Linux NAME_MAX (255). In practice K8s
// resource names are ≤253 chars (DNS subdomain) and data keys are short filenames.
func mountFilename(isSecret bool, resourceName, key string) string {
	prefix := "cm_"
	if isSecret {
		prefix = "secret_"
	}
	return prefix + resourceName + "_" + key
}

// detectDuplicateKeys checks whether any two discovered config resources of the
// same config type share a data key. Returns a list of human-readable warnings
// (one per duplicated key). Returns nil if no duplicates exist.
//
// This intentionally groups by (configType, filename) without distinguishing
// ConfigMap vs Secret. Even though mountFilename() guarantees distinct mount
// paths (cm_* vs secret_*), having the same data key in both a ConfigMap and
// a Secret of the same config type is still likely a user mistake — it may
// produce unexpected merged configuration.
func detectDuplicateKeys(configs []DiscoveredConfigResource) []string {
	type mountKey struct {
		configType string
		filename   string
	}
	seen := make(map[mountKey][]string)
	for _, cfg := range configs {
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		for key := range cfg.Data {
			mk := mountKey{configType: cfg.ConfigType, filename: key}
			seen[mk] = append(seen[mk], kind+"/"+cfg.Name)
		}
	}
	var warnings []string
	for mk, owners := range seen {
		if len(owners) > 1 {
			sort.Strings(owners)
			warnings = append(warnings, fmt.Sprintf(
				"data key %q (config type %q) exists in multiple resources: %v — all will be mounted at distinct paths (prefixed by resource name), verify this is intentional",
				mk.filename, mk.configType, owners,
			))
		}
	}
	sort.Strings(warnings)
	return warnings
}

// buildAppliedConfigStatus creates the status slice for discovered configs.
func buildAppliedConfigStatus(configs []DiscoveredConfigResource, validationStatus string) []v1alpha1.AppliedConfigStatus {
	if len(configs) == 0 {
		return nil
	}
	result := make([]v1alpha1.AppliedConfigStatus, 0, len(configs))
	for _, cfg := range configs {
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		result = append(result, v1alpha1.AppliedConfigStatus{
			Name:             cfg.Name,
			Kind:             kind,
			ConfigType:       cfg.ConfigType,
			ValidationStatus: validationStatus,
		})
	}
	return result
}

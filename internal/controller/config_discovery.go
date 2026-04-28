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

// Event reasons surfaced on managed ConfigMaps/Secrets (or the cluster CR
// for operator-level scan failures).
const (
	EventReasonEmptyClusterScope      = "EmptyClusterScope"
	EventReasonUnknownClusterInScope  = "UnknownClusterInScope"
	EventReasonUnknownConfigType      = "UnknownConfigType"
	EventReasonDuplicateConfigKey     = "DuplicateConfigKey"
	EventReasonScanFailed             = "ScanFailed"
	EventReasonLoggingConfigInvalid   = "LoggingConfigInvalid"
	EventReasonDuplicateLoggingConfig = "DuplicateLoggingConfig"
)

// Config type values matching the README.
const (
	ConfigTypeBase    = "base"
	ConfigTypeLicense = "license"
	ConfigTypeLogging = "logging"
)

const (
	MountPathBase    = "/opt/idsvr/etc/init/"
	MountPathLicense = "/opt/idsvr/etc/init/license/"
	MountPathLogging = "/opt/idsvr/etc/log4j2.xml"
)

const LoggingDataKey = "log4j2.xml"

// Volume name prefixes to avoid collisions with cluster-config.
const (
	volumePrefixConfigMap = "cfg-cm-"
	volumePrefixSecret    = "cfg-secret-"
	maxVolumeNameLength   = 63
)

// ErrUnknownConfigType is returned when a managed resource has an unrecognized
// curity.io/config-type annotation value.
var ErrUnknownConfigType = errors.New("unknown config type")

// DiscoveredManagedResource represents a ConfigMap or Secret found via
// label-based discovery (curity.io/managed=true).
type DiscoveredManagedResource struct {
	Name       string
	IsSecret   bool
	ConfigType string
	Data       map[string][]byte
	Object     client.Object // for use as Event involvedObject
}

// SkippedResource records a managed resource that was excluded from a
// discovery pass because its annotations are invalid. Surfaced so callers
// can emit a per-resource Warning Event without blocking reconciliation of
// the remaining valid resources — a single bad neighbor must not cascade
// into the entire managed-config feature going dark.
type SkippedResource struct {
	Name   string
	Kind   string        // "ConfigMap" or "Secret"
	Reason string        // stable, human-readable; used for EventRecorder dedup
	Object client.Object // for use as Event involvedObject
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

// discoverManagedResources lists ConfigMaps and Secrets labeled
// curity.io/managed=true in the given namespace, filtered by the
// curity.io/cluster annotation to those applying to clusterName.
// Operator-internal cluster-config secrets are filtered out.
//
// Returns (configs, skipped, error). Resources with an invalid
// curity.io/config-type annotation are reported via `skipped` and omitted
// from `configs` — one bad annotation must not block discovery of the
// remaining valid resources. Hard errors (e.g. List failures) still go in
// `error`. Results are sorted by name for deterministic ordering.
func discoverManagedResources(ctx context.Context, c client.Client, namespace, clusterName string) ([]DiscoveredManagedResource, []SkippedResource, error) {
	managedLabel := client.MatchingLabels{LabelManagedConfig: "true"}

	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, nil, fmt.Errorf("listing managed ConfigMaps: %w", err)
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, nil, fmt.Errorf("listing managed Secrets: %w", err)
	}

	result := make([]DiscoveredManagedResource, 0, len(configMaps.Items)+len(secrets.Items))
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
				Object: cm,
			})
			continue
		}
		data := make(map[string][]byte, len(cm.Data))
		for k, v := range cm.Data {
			data[k] = []byte(v)
		}
		result = append(result, DiscoveredManagedResource{
			Name:       cm.Name,
			IsSecret:   false,
			ConfigType: configType,
			Data:       data,
			Object:     cm,
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
				Object: s,
			})
			continue
		}
		result = append(result, DiscoveredManagedResource{
			Name:       s.Name,
			IsSecret:   true,
			ConfigType: configType,
			Data:       s.Data,
			Object:     s,
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
	Empty   bool          // annotation was present but empty
	Unknown []string      // cluster names in the annotation that don't exist
	Applied []string      // cluster names in the annotation that do exist (companion to Unknown)
	Object  client.Object // for use as Event involvedObject
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
	eval := func(kind, name string, ann map[string]string, obj client.Object) {
		raw, present := ann[AnnotationClusterScope]
		if !present {
			return
		}
		names, empty := parseClusterScope(raw)
		if empty {
			// Annotation present but yields no names (e.g. "", "  ", ",,").
			// Under applies-to-none semantics this mounts nowhere, which is
			// almost always a mistake — flag uniformly.
			issues = append(issues, ScopeIssue{Kind: kind, Name: name, Empty: true, Object: obj})
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
			issues = append(issues, ScopeIssue{Kind: kind, Name: name, Unknown: missing, Applied: applied, Object: obj})
		}
	}
	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		eval("ConfigMap", cm.Name, cm.Annotations, cm)
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if s.Labels["curity.io/component"] == "cluster-config" {
			continue
		}
		eval("Secret", s.Name, s.Annotations, s)
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
	case ConfigTypeLogging:
		return ConfigTypeLogging, nil
	default:
		return "", fmt.Errorf("%w: %q (valid: %q, %q, %q)",
			ErrUnknownConfigType, val, ConfigTypeBase, ConfigTypeLicense, ConfigTypeLogging)
	}
}

// mountPathForConfigType returns the mount path and whether it is a single-
// file leaf (logging) or a directory base shared by per-key mangled filenames
// (base, license).
func mountPathForConfigType(configType string) (path string, isLeaf bool) {
	switch configType {
	case ConfigTypeLicense:
		return MountPathLicense, false
	case ConfigTypeLogging:
		return MountPathLogging, true
	default:
		return MountPathBase, false
	}
}

// shouldMountConfig returns whether a node should receive a config of the
// given type. Logging mounts on every node regardless; base and license
// follow the admin-distributes pattern (admin-only when an admin exists).
func shouldMountConfig(nodeType v1alpha1.NodeType, adminExists bool, configType string) bool {
	if configType == ConfigTypeLogging {
		return true
	}
	if !adminExists {
		return true
	}
	return nodeType == v1alpha1.NodeTypeAdmin
}

// computeConfigHash returns a deterministic SHA256 hex string over the sorted
// config names and their data. Returns empty string for nil or empty configs.
func computeConfigHash(configs []DiscoveredManagedResource) string {
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

// DuplicateKeyWarning describes a single duplicated data key across managed
// resources of the same config type. Construct via newDuplicateKeyWarning,
// which enforces len(Owners) >= 2.
type DuplicateKeyWarning struct {
	Owners  []DuplicateKeyOwner
	Message string
}

// newDuplicateKeyWarning returns ok=false when len(owners) < 2, which would
// be an upstream invariant violation (a "duplicate" requires ≥ 2 owners).
func newDuplicateKeyWarning(owners []DuplicateKeyOwner, message string) (DuplicateKeyWarning, bool) {
	if len(owners) < 2 {
		return DuplicateKeyWarning{}, false
	}
	return DuplicateKeyWarning{Owners: owners, Message: message}, true
}

type DuplicateKeyOwner struct {
	Kind   string // "ConfigMap" or "Secret"
	Name   string
	Object client.Object // non-nil; for use as Event involvedObject
}

// detectDuplicateKeys checks whether any two discovered config resources of the
// same config type share a data key. Returns one warning per duplicated key,
// each carrying the live owner objects so callers can emit Warning Events
// keyed to each colliding ConfigMap/Secret (UID dedup applies). Returns nil
// if no duplicates exist.
//
// This intentionally groups by (configType, filename) without distinguishing
// ConfigMap vs Secret. Even though mountFilename() guarantees distinct mount
// paths (cm_* vs secret_*), having the same data key in both a ConfigMap and
// a Secret of the same config type is still likely a user mistake — it may
// produce unexpected merged configuration.
func detectDuplicateKeys(configs []DiscoveredManagedResource) []DuplicateKeyWarning {
	type mountKey struct {
		configType string
		filename   string
	}
	seen := make(map[mountKey][]DuplicateKeyOwner)
	for _, cfg := range configs {
		// Logging duplicates are reported via DuplicateLoggingConfig; skip
		// here so the same root cause isn't surfaced twice.
		if cfg.ConfigType == ConfigTypeLogging {
			continue
		}
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		for key := range cfg.Data {
			mk := mountKey{configType: cfg.ConfigType, filename: key}
			seen[mk] = append(seen[mk], DuplicateKeyOwner{Kind: kind, Name: cfg.Name, Object: cfg.Object})
		}
	}
	var warnings []DuplicateKeyWarning
	for mk, owners := range seen {
		if len(owners) <= 1 {
			continue // not a collision
		}
		sort.Slice(owners, func(i, j int) bool {
			if owners[i].Kind != owners[j].Kind {
				return owners[i].Kind < owners[j].Kind
			}
			return owners[i].Name < owners[j].Name
		})
		refs := make([]string, len(owners))
		for i, o := range owners {
			refs[i] = o.Kind + "/" + o.Name
		}
		w, ok := newDuplicateKeyWarning(owners, fmt.Sprintf(
			"data key %q (config type %q) exists in multiple resources: %v — all will be mounted at distinct paths (prefixed by resource name), verify this is intentional",
			mk.filename, mk.configType, refs,
		))
		if !ok {
			continue // invariant violated upstream; skip rather than emit malformed
		}
		warnings = append(warnings, w)
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Message < warnings[j].Message })
	return warnings
}

// validateLoggingResource enforces: exactly one data key named LoggingDataKey
// (case-sensitive). The byte-stable message goes into Events and status
// issues, so changes here affect Event dedup.
func validateLoggingResource(cfg DiscoveredManagedResource) (message string, ok bool) {
	keys := make([]string, 0, len(cfg.Data))
	for k := range cfg.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if len(keys) == 1 && keys[0] == LoggingDataKey {
		return "", true
	}
	return fmt.Sprintf(
		"Logging-typed resource must have exactly one data key named %q; got %d keys: %v",
		LoggingDataKey, len(keys), keys,
	), false
}

type DuplicateLoggingOwner struct {
	Kind   string
	Name   string
	Object client.Object
}

// detectDuplicateLoggingConfigs returns all logging-typed resources sorted
// by (Kind, Name) when more than one is present, or nil when ≤ 1.
// Picking one arbitrarily would surprise the user; the caller drops them all.
func detectDuplicateLoggingConfigs(configs []DiscoveredManagedResource) []DuplicateLoggingOwner {
	owners := make([]DuplicateLoggingOwner, 0)
	for _, cfg := range configs {
		if cfg.ConfigType != ConfigTypeLogging {
			continue
		}
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		owners = append(owners, DuplicateLoggingOwner{
			Kind:   kind,
			Name:   cfg.Name,
			Object: cfg.Object,
		})
	}
	if len(owners) <= 1 {
		return nil
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].Kind != owners[j].Kind {
			return owners[i].Kind < owners[j].Kind
		}
		return owners[i].Name < owners[j].Name
	})
	return owners
}

type LoggingValidationIssue struct {
	Kind        string
	Name        string
	Object      client.Object
	EventReason string
	Message     string
}

// applyLoggingValidations returns configs with invalid and duplicate
// logging resources removed plus the issues to surface. Called by both
// reconcilers so the mount set and the surfaced issues stay in sync.
func applyLoggingValidations(configs []DiscoveredManagedResource) ([]DiscoveredManagedResource, []LoggingValidationIssue) {
	issues := make([]LoggingValidationIssue, 0)

	valid := make([]DiscoveredManagedResource, 0, len(configs))
	for _, cfg := range configs {
		if cfg.ConfigType != ConfigTypeLogging {
			valid = append(valid, cfg)
			continue
		}
		if msg, ok := validateLoggingResource(cfg); !ok {
			kind := "ConfigMap"
			if cfg.IsSecret {
				kind = "Secret"
			}
			issues = append(issues, LoggingValidationIssue{
				Kind:        kind,
				Name:        cfg.Name,
				Object:      cfg.Object,
				EventReason: EventReasonLoggingConfigInvalid,
				Message:     msg,
			})
			continue
		}
		valid = append(valid, cfg)
	}

	dups := detectDuplicateLoggingConfigs(valid)
	if dups == nil {
		return valid, issues
	}

	names := make([]string, len(dups))
	for i, d := range dups {
		names[i] = d.Kind + "/" + d.Name
	}
	msg := fmt.Sprintf(
		"Cluster has %d applicable logging-typed resources: %v. None will be mounted; remove or scope all but one.",
		len(dups), names,
	)
	dupSet := make(map[string]struct{}, len(dups))
	for _, d := range dups {
		dupSet[d.Kind+"/"+d.Name] = struct{}{}
		issues = append(issues, LoggingValidationIssue{
			Kind:        d.Kind,
			Name:        d.Name,
			Object:      d.Object,
			EventReason: EventReasonDuplicateLoggingConfig,
			Message:     msg,
		})
	}
	filtered := make([]DiscoveredManagedResource, 0, len(valid)-len(dups))
	for _, cfg := range valid {
		if cfg.ConfigType != ConfigTypeLogging {
			filtered = append(filtered, cfg)
			continue
		}
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		if _, isDup := dupSet[kind+"/"+cfg.Name]; isDup {
			continue
		}
		filtered = append(filtered, cfg)
	}
	return filtered, issues
}

// buildAppliedManagedResources creates the status slice for discovered
// managed ConfigMaps and Secrets that have been mounted.
func buildAppliedManagedResources(configs []DiscoveredManagedResource) []v1alpha1.AppliedManagedResource {
	if len(configs) == 0 {
		return nil
	}
	result := make([]v1alpha1.AppliedManagedResource, 0, len(configs))
	for _, cfg := range configs {
		kind := "ConfigMap"
		if cfg.IsSecret {
			kind = "Secret"
		}
		result = append(result, v1alpha1.AppliedManagedResource{
			Name:       cfg.Name,
			Kind:       kind,
			ConfigType: cfg.ConfigType,
		})
	}
	return result
}

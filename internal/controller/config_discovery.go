package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Labels and annotations for config discovery.
const (
	LabelManagedConfig   = "curity.io/managed"
	AnnotationConfigType = "curity.io/config-type"
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

// discoverConfigResources lists ConfigMaps and Secrets labeled
// curity.io/managed=true in the given namespace. It filters out the
// operator-internal cluster-config secret and validates config type
// annotations. Results are sorted by name for deterministic ordering.
func discoverConfigResources(ctx context.Context, c client.Client, namespace string) ([]DiscoveredConfigResource, error) {
	managedLabel := client.MatchingLabels{LabelManagedConfig: "true"}

	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, fmt.Errorf("listing managed ConfigMaps: %w", err)
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.InNamespace(namespace), managedLabel); err != nil {
		return nil, fmt.Errorf("listing managed Secrets: %w", err)
	}

	result := make([]DiscoveredConfigResource, 0, len(configMaps.Items)+len(secrets.Items))

	for i := range configMaps.Items {
		cm := &configMaps.Items[i]
		configType, err := resolveConfigType(cm.Annotations)
		if err != nil {
			return nil, fmt.Errorf("configmap %q: %w", cm.Name, err)
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
		configType, err := resolveConfigType(s.Annotations)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", s.Name, err)
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

	return result, nil
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

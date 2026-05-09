package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// annotationPackagesHash is stamped on the Deployment pod template and
// changes when spec.packages changes (URL, TLS refs, auth refs, mountPath,
// or order). Mirrors the curity.io/managed-configs-hash mechanism so that
// any spec edit triggers a rolling restart on the next reconcile.
const annotationPackagesHash = "curity.io/packages-hash"

// DefaultPackageFetcherImage is the alpine image used for the per-package
// init container. The Pre-work probe (see PLAN-packages-feature.md) showed
// the Curity image lacks curl/wget/unzip; alpine:3.19 is the smallest
// public image whose package repos provide a full curl (mTLS, redirects)
// and unzip. Override at operator-deployment time via PACKAGE_FETCHER_IMAGE.
const DefaultPackageFetcherImage = "alpine:3.19"

// EnvPackageFetcherImage is the operator-level env var that overrides
// DefaultPackageFetcherImage at startup. Used by air-gap operators who
// mirror to a private registry.
const EnvPackageFetcherImage = "PACKAGE_FETCHER_IMAGE"

// packageVolumeSizeLimit caps per-package emptyDir size. Limits the
// blast radius of a misbehaving URL (zip-bomb, runaway download) to
// 256MiB of node ephemeral storage per package.
var packageVolumeSizeLimit = resource.MustParse("256Mi")

// ResolvePackageFetcherImage reads $PACKAGE_FETCHER_IMAGE once at
// controller construction (called from cmd/manager/main.go), returning
// the override if set, otherwise DefaultPackageFetcherImage. The result
// is stored on IdentityServerNodeReconciler.PackageFetcherImage and
// threaded through buildDeployment so reconciles do not re-read env.
func ResolvePackageFetcherImage() string {
	if v := os.Getenv(EnvPackageFetcherImage); v != "" {
		return v
	}
	return DefaultPackageFetcherImage
}

// packageVolumeName returns the emptyDir volume name for the n-th package.
func packageVolumeName(idx int) string {
	return fmt.Sprintf("pkg-%d", idx)
}

// packageTLSVolumeName returns the projected-Secret volume name carrying
// the TLS material for the n-th package, if any.
func packageTLSVolumeName(idx int) string {
	return fmt.Sprintf("pkg-%d-tls", idx)
}

// packageInitContainerName returns the init container name for the n-th
// package. Matches the K8s container-name DNS-1123 label rules.
func packageInitContainerName(idx int) string {
	return fmt.Sprintf("package-fetch-%d", idx)
}

// packageEnabledTLS reports whether TLS customization should be applied
// for this package. Per the proposal yaml, TLS sub-fields are only honored
// when TLS.Enabled is true; when false (default), the system trust store
// is used and ca/clientCert/skipVerify are ignored.
func packageEnabledTLS(p v1alpha1.PackageSpec) bool {
	return p.Source.TLS != nil && p.Source.TLS.Enabled
}

// hasPackageTLSVolume reports whether the package will produce a TLS
// projected volume — i.e. TLS is enabled AND at least one of CA or
// ClientCert references a Secret. Single source of truth used by both
// buildPackageVolumes (decides whether to emit a volume) and
// buildPackageInitVolumeMounts (decides whether to mount /tls).
func hasPackageTLSVolume(p v1alpha1.PackageSpec) bool {
	if !packageEnabledTLS(p) {
		return false
	}
	tls := p.Source.TLS
	return tls.CA != nil || tls.ClientCert != nil
}

// derivePrivateKeyKey returns the Secret key holding the matching private
// key for a TLS client cert whose cert is at certKey. Convention: the
// trailing "crt" is replaced with "key" (so "tls.crt" -> "tls.key",
// "client.crt" -> "client.key"). Falls back to "tls.key" when certKey
// does not end in "crt" so the kubernetes.io/tls Secret convention works
// even with unconventional cert key names.
func derivePrivateKeyKey(certKey string) string {
	if strings.HasSuffix(certKey, "crt") {
		return certKey[:len(certKey)-3] + "key"
	}
	return "tls.key"
}

// buildPackageVolumes returns the volumes to attach to the pod template for
// every package: one emptyDir per package (for the unpacked archive) and
// one projected-Secret per package whose TLS block is enabled and references
// CA or client-cert material.
func buildPackageVolumes(packages []v1alpha1.PackageSpec) []corev1.Volume {
	if len(packages) == 0 {
		return nil
	}
	volumes := make([]corev1.Volume, 0, len(packages))
	for i, p := range packages {
		volumes = append(volumes, corev1.Volume{
			Name: packageVolumeName(i),
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: &packageVolumeSizeLimit,
				},
			},
		})
		if tlsVol := buildPackageTLSVolume(p, i); tlsVol != nil {
			volumes = append(volumes, *tlsVol)
		}
	}
	return volumes
}

// buildPackageTLSVolume returns a projected volume bundling CA cert and/or
// mTLS client cert+key for the n-th package. Returns nil when TLS is not
// enabled or no Secret refs are set — guarded by hasPackageTLSVolume so
// the same predicate gates volume emission and /tls mount injection.
func buildPackageTLSVolume(p v1alpha1.PackageSpec, idx int) *corev1.Volume {
	if !hasPackageTLSVolume(p) {
		return nil
	}
	tls := p.Source.TLS
	sources := make([]corev1.VolumeProjection, 0, 2)
	if tls.CA != nil {
		sources = append(sources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: tls.CA.SecretRef.Name},
				Items: []corev1.KeyToPath{
					{Key: tls.CA.SecretRef.Key, Path: "ca.crt"},
				},
			},
		})
	}
	if tls.ClientCert != nil {
		certKey := tls.ClientCert.SecretRef.Key
		sources = append(sources, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: tls.ClientCert.SecretRef.Name},
				Items: []corev1.KeyToPath{
					{Key: certKey, Path: "tls.crt"},
					{Key: derivePrivateKeyKey(certKey), Path: "tls.key"},
				},
			},
		})
	}
	return &corev1.Volume{
		Name: packageTLSVolumeName(idx),
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: ptr.To(int32(0o400)),
				Sources:     sources,
			},
		},
	}
}

// buildPackageVolumeMounts returns the mounts the main Curity container
// needs so the unpacked archive is visible at each package's MountPath.
func buildPackageVolumeMounts(packages []v1alpha1.PackageSpec) []corev1.VolumeMount {
	if len(packages) == 0 {
		return nil
	}
	mounts := make([]corev1.VolumeMount, 0, len(packages))
	for i, p := range packages {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      packageVolumeName(i),
			MountPath: p.MountPath,
			ReadOnly:  true,
		})
	}
	return mounts
}

// buildPackageInitContainers returns one init container per package. Each
// runs the operator-generated download-and-unpack script with TLS material
// projected as files and auth credentials projected as env vars (consumed
// via stdin or netrc so they never appear in argv or curl-verbose output).
// The image is supplied by the reconciler (resolved once at controller
// construction via ResolvePackageFetcherImage) so reconciles never re-read
// $PACKAGE_FETCHER_IMAGE from process env.
func buildPackageInitContainers(packages []v1alpha1.PackageSpec, image string) []corev1.Container {
	if len(packages) == 0 {
		return nil
	}
	containers := make([]corev1.Container, 0, len(packages))
	for i, p := range packages {
		containers = append(containers, buildPackageInitContainer(p, i, image))
	}
	return containers
}

// buildPackageInitContainer builds the single init container for the n-th
// package. The script runs as root (UID 0) per-container so apk add can
// write to /var/lib/apk; the unpacked files in the emptyDir are picked up
// by the main container via the pod-level FSGroup.
func buildPackageInitContainer(p v1alpha1.PackageSpec, idx int, image string) corev1.Container {
	c := corev1.Container{
		Name:            packageInitContainerName(idx),
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{buildPackageDownloadScript(p, idx)},
		Env:             buildPackageInitEnv(p),
		VolumeMounts:    buildPackageInitVolumeMounts(p, idx),
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:  ptr.To(int64(0)),
			RunAsGroup: ptr.To(int64(0)),
		},
	}
	return c
}

// buildPackageInitEnv returns the env vars the init-container script reads.
// PKG_URL holds the URL (passed as a value, NOT interpolated into the
// script body — eliminates shell-command-injection via URL). PKG_HOST is
// only set for basic-auth packages and holds the hostname (and port) for
// the netrc machine line. Auth credentials project Secret keys via
// secretKeyRef (BEARER_TOKEN, BASIC_AUTH_USER, BASIC_AUTH_PASS) and the
// script consumes them via stdin / netrc so they never appear on argv.
func buildPackageInitEnv(p v1alpha1.PackageSpec) []corev1.EnvVar {
	envs := []corev1.EnvVar{
		// URL is passed as an env var (NOT interpolated into the script
		// source) so shell metacharacters in the URL cannot break out of
		// the curl invocation. The script always references "$PKG_URL"
		// inside double quotes, which preserves the value verbatim.
		{Name: "PKG_URL", Value: p.Source.URL},
	}
	if p.Source.Auth == nil {
		return envs
	}
	if bt := p.Source.Auth.BearerToken; bt != nil {
		envs = append(envs, corev1.EnvVar{
			Name: "BEARER_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: bt.SecretRef.Name},
					Key:                  bt.SecretRef.Key,
				},
			},
		})
	}
	if ba := p.Source.Auth.BasicAuth; ba != nil {
		// PKG_HOST is the netrc "machine" field. Computed in Go via
		// url.Parse so we don't have to extract host with shell sed at
		// runtime — that older approach broke on URLs containing the sed
		// delimiter ('#') and would have also been an injection sink.
		envs = append(envs,
			corev1.EnvVar{Name: "PKG_HOST", Value: packageURLHost(p.Source.URL)},
			corev1.EnvVar{
				Name: "BASIC_AUTH_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: ba.SecretRef.Name},
						Key:                  ba.SecretRef.UsernameKey,
					},
				},
			},
			corev1.EnvVar{
				Name: "BASIC_AUTH_PASS",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: ba.SecretRef.Name},
						Key:                  ba.SecretRef.PasswordKey,
					},
				},
			},
		)
	}
	return envs
}

// packageURLHost returns the hostname (NO port) of a package URL — the
// value that goes into the netrc `machine` line for basic auth. curl
// strips the port from the URL's host before matching against netrc, so
// `machine host:port login ...` would never match a request for
// `http://host:port/...`. Use Hostname() not Host. CRD validation
// ensures the URL parses; on the rare parse-failure path we return ""
// and let curl fail at runtime so the init container exits non-zero
// with a visible error.
func packageURLHost(u string) string {
	parsed, err := url.Parse(u)
	if err != nil || parsed == nil {
		return ""
	}
	return parsed.Hostname()
}

// buildPackageInitVolumeMounts returns the init container's mounts:
// the package's emptyDir at /pkg, plus the TLS projection at /tls when
// enabled. Auth credentials are env vars, not files.
func buildPackageInitVolumeMounts(p v1alpha1.PackageSpec, idx int) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{
			Name:      packageVolumeName(idx),
			MountPath: "/pkg",
		},
	}
	if hasPackageTLSVolume(p) {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      packageTLSVolumeName(idx),
			MountPath: "/tls",
			ReadOnly:  true,
		})
	}
	return mounts
}

// buildPackageDownloadScript returns the /bin/sh -c body for the n-th
// package's init container. The script is generated by the operator (not
// user-supplied) so it can be audited centrally. It:
//   - installs curl + unzip + ca-certificates via apk
//   - downloads the URL with -L (follow redirects), -fsS (fail on HTTP error,
//     silent except errors), --max-time 120, --max-filesize 268435456 (256MB)
//   - applies TLS flags only when tls.enabled is true
//   - feeds bearer tokens via stdin (--header @-) and basic-auth via netrc
//     so credentials never appear in process argv or curl's output
//   - unpacks into /pkg
//   - exits non-zero on any failure so the pod stays in Init:Error
func buildPackageDownloadScript(p v1alpha1.PackageSpec, idx int) string {
	var sb strings.Builder
	sb.WriteString("set -e\n")
	// >/dev/null silences apk's verbose progress lines, but stderr is
	// deliberately NOT redirected — if Alpine repos are unreachable
	// (NetworkPolicy, air-gap, DNS hiccup), the failure reason surfaces
	// in `kubectl logs <pod> -c package-fetch-N` instead of a silent
	// Init:Error with no breadcrumb.
	sb.WriteString("apk add --no-cache curl unzip ca-certificates >/dev/null\n")
	sb.WriteString("mkdir -p /pkg\n")

	curlArgs := `-fsSL --max-time 120 --max-filesize 268435456 -o /tmp/pkg.zip`

	// TLS flags
	tlsFlags := ""
	if packageEnabledTLS(p) {
		tls := p.Source.TLS
		switch {
		case tls.SkipVerify:
			tlsFlags = " -k"
		default:
			if tls.CA != nil {
				tlsFlags += " --cacert /tls/ca.crt"
			}
			if tls.ClientCert != nil {
				tlsFlags += " --cert /tls/tls.crt --key /tls/tls.key"
			}
		}
	}

	// Auth handling: prefer stdin (bearer) or netrc (basic) so credentials
	// never appear in argv. Emit the curl invocation in the variant that
	// matches what's set; only one of bearer/basic can be set per CRD CEL.
	//
	// SECURITY: the URL is referenced as "$PKG_URL" (env var set by
	// buildPackageInitEnv), NOT interpolated into the script body via
	// fmt.Sprintf. That eliminates shell-command-injection via URLs
	// containing shell metacharacters (e.g. `"; rm -rf /; #`). The CRD
	// Pattern only enforces ^https?:// and MaxLength=2048 — it does not
	// forbid quotes / backticks / dollar signs. Defense lives in code.
	auth := p.Source.Auth
	switch {
	case auth != nil && auth.BearerToken != nil:
		// Bearer token via stdin (--header @-) so it never hits cmdline.
		// Format string is the constant "%s\n" so Secret bytes never
		// reach printf's format parser.
		sb.WriteString("printf '%s\\n' \"Authorization: Bearer $BEARER_TOKEN\" | curl")
		sb.WriteString(tlsFlags)
		sb.WriteString(" --header @- ")
		sb.WriteString(curlArgs)
		sb.WriteString(" \"$PKG_URL\"\n")
	case auth != nil && auth.BasicAuth != nil:
		// Basic auth via netrc file (mode 0600). The netrc line is built
		// with `printf '%s\n' "..."` — constant format string, single
		// argument — so Secret bytes are NEVER interpreted as printf
		// format directives. Host comes from a Go-computed PKG_HOST env
		// var (see packageURLHost) instead of runtime sed extraction.
		sb.WriteString("touch /tmp/netrc && chmod 600 /tmp/netrc\n")
		sb.WriteString("printf '%s\\n' \"machine $PKG_HOST login $BASIC_AUTH_USER password $BASIC_AUTH_PASS\" > /tmp/netrc\n")
		sb.WriteString("curl")
		sb.WriteString(tlsFlags)
		sb.WriteString(" --netrc-file /tmp/netrc ")
		sb.WriteString(curlArgs)
		sb.WriteString(" \"$PKG_URL\"\n")
	default:
		sb.WriteString("curl")
		sb.WriteString(tlsFlags)
		sb.WriteString(" ")
		sb.WriteString(curlArgs)
		sb.WriteString(" \"$PKG_URL\"\n")
	}

	sb.WriteString("unzip -q /tmp/pkg.zip -d /pkg\n")
	fmt.Fprintf(&sb, "echo \"package-fetch-%d: extracted to /pkg\"\n", idx)
	return sb.String()
}

// computePackagesHash returns a hex-encoded full SHA-256 of the PackageSpec
// slice. Matches the cluster-config-hash convention (also full SHA-256) so
// future readers don't have to wonder why one annotation is 16 chars and
// another is 64. Hashes refs (Secret name + key) and structural fields,
// NOT Secret contents — so rotating a token in place does not trigger a
// rolling restart. Returns "" for nil/empty input.
//
// Uses encoding/json with the spec's tag names to ensure every field
// in PackageSpec contributes to the hash; if a future field is added to
// PackageSpec but not the hash function, json.Marshal still picks it up.
func computePackagesHash(packages []v1alpha1.PackageSpec) string {
	if len(packages) == 0 {
		return ""
	}
	b, err := json.Marshal(packages)
	if err != nil {
		// json.Marshal of a struct slice with string/bool fields cannot
		// fail under any input the K8s API server would accept; surface
		// the error if it ever does.
		return fmt.Sprintf("err:%v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

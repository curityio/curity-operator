package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// packageInitContainerResources caps the CPU and memory available to
// every package-fetch init container. Without this, a zip-bomb, runaway
// `apk add`, or a malicious URL response could consume node-level
// CPU/memory until eviction (the emptyDir size limit caps disk only).
//
// Sizing rationale:
//   - CPU: requests=50m, limits=500m. The init is a one-shot curl +
//     unzip; legitimate work is bursty for ~1-5s. 500m allows that burst
//     without letting a CPU-spinning shell exploit starve the node.
//   - Memory: requests=64Mi, limits=256Mi. apk index + curl runtime
//     ~30-50Mi RSS; unzip streams (RSS stays small even for 256Mi ZIPs
//     because emptyDir is disk-backed, not tmpfs). 256Mi limit matches
//     the volume cap so the worst-case blast radius is symmetric.
var packageInitContainerResources = corev1.ResourceRequirements{
	Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	},
	Limits: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("500m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	},
}

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
		// Bound CPU/memory so a zip-bomb, runaway apk install, or
		// malicious URL response cannot consume node resources until
		// eviction. See packageInitContainerResources for sizing
		// rationale. Disk is bounded separately by the emptyDir
		// SizeLimit on the package volume (packageVolumeSizeLimit).
		Resources: packageInitContainerResources,
		// Defense-in-depth: even though the init runs as UID 0 (apk
		// needs root to write /var/lib/apk), strip every Linux
		// capability and deny setuid escalation. apk + curl + unzip
		// require zero caps; none of them call setuid binaries. A
		// compromised package URL that escapes the script still lands
		// in a container that can't SYS_ADMIN, can't NET_RAW, and
		// can't escalate.
		//
		// SeccompProfile is intentionally NOT set: OpenShift's anyuid
		// SCC (which the operator requires for runAsUser=0) rejects
		// pods that set seccompProfile, and no standard SCC allows
		// both seccomp AND UID 0 simultaneously. On standard K8s with
		// PodSecurityAdmission "restricted", the namespace policy
		// injects seccompProfile=RuntimeDefault when the pod does not
		// specify one — so we lose nothing on PSA clusters and
		// preserve OpenShift compatibility. Users who want explicit
		// operator-managed seccomp on non-PSA clusters can pursue an
		// env-var opt-in as a follow-up.
		//
		// readOnlyRootFilesystem is also NOT set — apk writes to
		// /var/lib/apk, /usr/bin, /etc/apk; enabling it would break
		// the install step. A future image with curl + unzip baked in
		// would allow readOnly + dropping the apk step entirely.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(int64(0)),
			RunAsGroup:               ptr.To(int64(0)),
			AllowPrivilegeEscalation: ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
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

	// Wrap unzip with "|| exit 100" so an archive failure is distinguishable
	// from a curl failure: curl reserves exits 1-93, so the script's exit
	// becomes 100 only when the downloaded artifact is not a valid ZIP
	// (server returned HTML, partial download, corrupt archive). The
	// PackagesReady translator maps exit 100 to ReasonPackageInvalidArchive.
	sb.WriteString("unzip -q /tmp/pkg.zip -d /pkg || exit 100\n")
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

// packageURLMessageMaxLen caps URLs embedded in condition messages so the
// total stays well under K8s's 32KiB condition-message limit even with
// long URLs and many historical condition transitions.
const packageURLMessageMaxLen = 200

// truncatePackageURL trims a URL for embedding in a condition message,
// appending "..." when the source exceeds packageURLMessageMaxLen.
func truncatePackageURL(u string) string {
	if len(u) <= packageURLMessageMaxLen {
		return u
	}
	return u[:packageURLMessageMaxLen] + "..."
}

// formatPackageMessage produces the canonical message format shared by both
// the pre-check leg and the pod-watch translator. Embedding the index, mount
// path, and (truncated) URL lets a user reading `kubectl describe` map the
// failure straight back to a `spec.packages[N]` entry.
func formatPackageMessage(idx int, p v1alpha1.PackageSpec, detail string) string {
	return fmt.Sprintf("package-fetch-%d (index=%d, mountPath=%s, url=%s): %s",
		idx, idx, p.MountPath, truncatePackageURL(p.Source.URL), detail)
}

// secretRefCheckResult classifies one Secret reference's pre-check outcome.
type secretRefCheckResult struct {
	reason   string // "" on success or transient error
	detail   string // populated only when reason != ""
	retryErr error  // non-nil only on transient (non-IsNotFound) errors
}

// isFailure reports whether the result represents either a definitive
// failure (reason set) or a transient error (retryErr set) that should
// stop the iteration. Callers still need to inspect retryErr vs reason
// to know how to propagate, but the predicate centralises the precedence
// rule so call sites don't have to repeat it.
func (r secretRefCheckResult) isFailure() bool {
	return r.retryErr != nil || r.reason != ""
}

// checkSecretKeys looks up a Secret by name and verifies that every key in
// requiredKeys is present in Secret.Data. Returns:
//   - {reason: "", retryErr: nil} on success.
//   - {reason: ReasonPackageSecretMissing} when the Secret is not found.
//   - {reason: ReasonPackageSecretKeyMissing} when a required key is absent.
//   - {retryErr: err} on any other API error — pre-check is inconclusive,
//     caller should propagate the error so controller-runtime backs off
//     instead of flipping the condition to False on a transient blip.
func checkSecretKeys(ctx context.Context, r client.Reader, namespace, secretName string, requiredKeys []string) secretRefCheckResult {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return secretRefCheckResult{
				reason: v1alpha1.ReasonPackageSecretMissing,
				detail: fmt.Sprintf("Secret %q not found", secretName),
			}
		}
		return secretRefCheckResult{retryErr: fmt.Errorf("getting Secret %q: %w", secretName, err)}
	}
	for _, key := range requiredKeys {
		if _, ok := secret.Data[key]; !ok {
			return secretRefCheckResult{
				reason: v1alpha1.ReasonPackageSecretKeyMissing,
				detail: fmt.Sprintf("key %q not found in Secret %q", key, secretName),
			}
		}
	}
	return secretRefCheckResult{}
}

// checkPackageSecrets validates that every Secret reference in spec.packages
// resolves to an existing Secret containing every named key. Called from the
// node reconciler before stamping a Deployment that would otherwise fail at
// pod-start time with an opaque kubelet error.
//
// Return values:
//   - (reason="", message="", retryErr=nil): all refs resolve, pre-check passed.
//   - (reason=Reason..., message="<formatted>", retryErr=nil): first definitive
//     failure (Secret missing or required key missing). Iteration stops at the
//     first failure — serial-fix UX, matching the pod-watch translator.
//   - (reason="", message="", retryErr=err): a non-IsNotFound API error
//     occurred. Caller MUST propagate err to controller-runtime (exponential
//     backoff requeue) without changing the condition — pre-check is
//     inconclusive, not failed (plan D13).
func checkPackageSecrets(ctx context.Context, r client.Reader, packages []v1alpha1.PackageSpec, namespace string) (reason, message string, retryErr error) {
	for i, p := range packages {
		if res := checkPackageRefs(ctx, r, namespace, p); res.isFailure() {
			if res.retryErr != nil {
				return "", "", res.retryErr
			}
			return res.reason, formatPackageMessage(i, p, res.detail), nil
		}
	}
	return "", "", nil
}

// checkPackageRefs walks all Secret references attached to one PackageSpec,
// returning the first failure or an empty result on success. Split from
// checkPackageSecrets so each ref-type's logic stays focused.
//
// TLS references are gated on packageEnabledTLS — when tls.enabled=false
// (the default), the runtime ignores ca and clientCert refs (no projected
// volume, no --cacert/--cert flags), so the pre-check must skip them too
// or it produces false positives on stale refs that the user disabled.
func checkPackageRefs(ctx context.Context, r client.Reader, namespace string, p v1alpha1.PackageSpec) secretRefCheckResult {
	if packageEnabledTLS(p) {
		if p.Source.TLS.CA != nil {
			ref := p.Source.TLS.CA.SecretRef
			if res := checkSecretKeys(ctx, r, namespace, ref.Name, []string{ref.Key}); res.isFailure() {
				return res
			}
		}
		if p.Source.TLS.ClientCert != nil {
			ref := p.Source.TLS.ClientCert.SecretRef
			// Pre-check requires both the cert key and the derived private
			// key sibling — same convention as buildPackageTLSVolume.
			keys := []string{ref.Key, derivePrivateKeyKey(ref.Key)}
			if res := checkSecretKeys(ctx, r, namespace, ref.Name, keys); res.isFailure() {
				return res
			}
		}
	}
	if p.Source.Auth != nil {
		if p.Source.Auth.BearerToken != nil {
			ref := p.Source.Auth.BearerToken.SecretRef
			if res := checkSecretKeys(ctx, r, namespace, ref.Name, []string{ref.Key}); res.isFailure() {
				return res
			}
		}
		if p.Source.Auth.BasicAuth != nil {
			ref := p.Source.Auth.BasicAuth.SecretRef
			if res := checkSecretKeys(ctx, r, namespace, ref.Name, []string{ref.UsernameKey, ref.PasswordKey}); res.isFailure() {
				return res
			}
		}
	}
	return secretRefCheckResult{}
}

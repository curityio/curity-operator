package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// curl exit codes the translator maps to specific PackagesReady reasons. Any
// other non-zero exit (DNS=6, TCP refused=7, timeout=28, size limit=63, etc.)
// falls through to ReasonPackageFetchFailed with the verbatim exit code in
// the message. Exit 100 is operator-emitted by the download script's
// `unzip ... || exit 100` wrapper (see buildPackageDownloadScript).
const (
	exitCodeHTTPError       = 22  // curl --fail (-f) on HTTP 4xx/5xx
	exitCodeClientCertBad   = 58  // curl: unable to use client cert file
	exitCodeTLSVerifyFailed = 60  // curl: SSL certificate problem
	exitCodeSSLEngineInit   = 82  // curl: SSL engine could not be initialized
	exitCodeUnzipFailure    = 100 // operator-emitted; see download script
)

// translatePackagesReadyResult bundles the outcome of running the pod-watch
// translator over a set of pods. The set flag distinguishes "no condition
// should be set" from "set PackagesReady=True" — both leave the condition
// off the True path when no current pods have package-fetch evidence, but
// only one means the operator has *seen* a successful fetch.
type translatePackagesReadyResult struct {
	status  metav1.ConditionStatus // True or False
	reason  string                 // exact reason string for setCondition
	message string                 // formatted via formatPackageMessage
	set     bool                   // false = omit the condition (per F5c)
}

// translatePackagesReady inspects the init-container statuses of every pod
// owned by the current Deployment's most-recent ReplicaSet and decides
// what PackagesReady value (if any) to set on the node. It is the runtime
// arm of the hybrid pre-check/pod-watch design — pre-check covers spec-time
// failures, this leg covers pod-start-time failures (curl, unzip, kubelet
// mount errors, image-pull errors).
//
// Inputs are filtered before this function runs (caller selects pods by
// app.kubernetes.io/instance label = OwnedResourceName(cluster, node)).
//
// expectedHash is the cluster's current packagesHash (the value the operator
// would stamp on a freshly-rendered Deployment template). Only pods whose
// curity.io/packages-hash annotation matches contribute to the decision —
// this closes the rolling-update false-positive window where the OLD
// ReplicaSet's still-serving pods (with the OLD hash) would otherwise
// surface as PackagesReady=True for the NEW spec.
//
// Stale-pod-ghost mitigation. Two filters compose:
//
//  1. Hash filter: pods whose curity.io/packages-hash annotation does not
//     equal expectedHash are ignored. This handles the rolling-update
//     window and the rare oscillation case (spec A → B → A produces a
//     stale crashlooping pod whose RS is being scaled down).
//  2. ReplicaSet filter: pods are grouped by controller ReplicaSet UID;
//     only the group with the newest max-CreationTimestamp wins. Defends
//     against the case where two ReplicaSets carry the same hash (rare,
//     but possible during fast oscillation or controller restart).
//
// Pods with DeletionTimestamp set are skipped (they are about to vanish).
//
// Per F5 (Decisions D4), this function returns set=false when:
//   - no surviving pods match expectedHash yet (initial rollout, fresh hash),
//   - all matching pods have DeletionTimestamp set (cluster is being torn down),
//   - the current ReplicaSet's pods don't yet have any package-fetch-* entries
//     in their InitContainerStatuses (still pulling images, very brief).
//
// The caller treats set=false as "leave PackagesReady at its prior value"
// — which after this PR is typically PackagesPending (set preemptively by
// the reconciler when the hash flips) or AllPackagesFetched (from the
// previous steady state).
func translatePackagesReady(pods []corev1.Pod, expectedHash string, packages []v1alpha1.PackageSpec) translatePackagesReadyResult {
	matchingHash := podsMatchingPackagesHash(pods, expectedHash)
	if len(matchingHash) == 0 {
		return translatePackagesReadyResult{set: false}
	}
	current := currentReplicaSetPods(matchingHash)
	if len(current) == 0 {
		return translatePackagesReadyResult{set: false}
	}

	successCount := 0
	var sawAnyPackageContainer bool

	for i := range current {
		for j := range current[i].Status.InitContainerStatuses {
			ics := &current[i].Status.InitContainerStatuses[j]
			if !strings.HasPrefix(ics.Name, packageFetchContainerNamePrefix) {
				continue
			}
			sawAnyPackageContainer = true

			idx, ok := parsePackageFetchIndex(ics.Name)
			if !ok {
				// Defensive — the predicate ensures all matching entries
				// have a "package-fetch-<int>" name, but unparseable names
				// fall through to the catch-all reason rather than panic.
				idx = -1
			}

			if reason, detail, isFailure := classifyInitContainer(ics); isFailure {
				return translatePackagesReadyResult{
					status:  metav1.ConditionFalse,
					reason:  reason,
					message: formatPodPackageMessage(idx, packages, ics.Name, detail),
					set:     true,
				}
			}

			if ics.State.Terminated != nil && ics.State.Terminated.ExitCode == 0 {
				successCount++
			}
		}
	}

	if !sawAnyPackageContainer {
		// Pods exist but their init containers haven't reached the
		// package-fetch stage yet (image pull, schedule wait). Omit the
		// condition rather than fabricate one — see F5c.
		return translatePackagesReadyResult{set: false}
	}

	if successCount == 0 {
		// Every package-fetch container is in a non-failure non-terminated
		// state (Waiting=PodInitializing, Running). Omit until we see
		// either a definitive failure or a clean Terminated=0.
		return translatePackagesReadyResult{set: false}
	}

	return translatePackagesReadyResult{
		status:  metav1.ConditionTrue,
		reason:  v1alpha1.ReasonAllPackagesFetched,
		message: fmt.Sprintf("%d package-fetch init container(s) succeeded", successCount),
		set:     true,
	}
}

// podsMatchingPackagesHash returns the subset of pods whose
// curity.io/packages-hash annotation equals expectedHash. The annotation is
// stamped on the pod template at Deployment-render time and inherited by
// pods, so it identifies which packages spec a given pod was launched with.
// During a rolling update, old pods retain the old hash while new pods
// carry the new hash — this filter strips out the old ones so the
// translator only reasons about the spec the operator is currently
// claiming. expectedHash="" returns no pods (defensive: a packages-empty
// reconcile should not reach this function anyway).
func podsMatchingPackagesHash(pods []corev1.Pod, expectedHash string) []corev1.Pod {
	if expectedHash == "" {
		return nil
	}
	out := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		if pods[i].Annotations[annotationPackagesHash] == expectedHash {
			out = append(out, pods[i])
		}
	}
	return out
}

// currentReplicaSetPods returns the subset of pods that belong to the most
// recent ReplicaSet (by max pod CreationTimestamp). Pods being deleted are
// excluded. Returns nil if no eligible pods remain.
func currentReplicaSetPods(pods []corev1.Pod) []corev1.Pod {
	// Group surviving pods by controlling ReplicaSet UID; track the
	// newest CreationTimestamp per group to find the "current" generation.
	type group struct {
		pods   []corev1.Pod
		newest metav1.Time
		gotUID types.UID
	}
	groups := map[types.UID]*group{}

	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		rsUID, ok := controllerReplicaSetUID(p.OwnerReferences)
		if !ok {
			continue
		}
		g, exists := groups[rsUID]
		if !exists {
			g = &group{gotUID: rsUID}
			groups[rsUID] = g
		}
		g.pods = append(g.pods, *p)
		if p.CreationTimestamp.After(g.newest.Time) {
			g.newest = p.CreationTimestamp
		}
	}
	if len(groups) == 0 {
		return nil
	}

	// Pick the group with the latest newest-pod CreationTimestamp.
	// Ties broken by UID lexical order — deterministic, though ties in
	// pod CreationTimestamp are extremely unlikely (1s resolution).
	var winner *group
	for _, g := range groups {
		switch {
		case winner == nil:
			winner = g
		case g.newest.After(winner.newest.Time):
			winner = g
		case g.newest.Equal(&winner.newest) && string(g.gotUID) < string(winner.gotUID):
			winner = g
		}
	}
	return winner.pods
}

// controllerReplicaSetUID returns the UID of the controller OwnerReference
// of kind ReplicaSet, if any.
func controllerReplicaSetUID(refs []metav1.OwnerReference) (types.UID, bool) {
	for i := range refs {
		ref := &refs[i]
		if ref.Kind == "ReplicaSet" && ref.Controller != nil && *ref.Controller {
			return ref.UID, true
		}
	}
	return "", false
}

// classifyInitContainer maps a single InitContainerStatus to a (reason,
// detail) tuple. Returns isFailure=true only when the status represents a
// definitive failure — waiting states like PodInitializing and Running
// states return isFailure=false so the translator does not flip on
// transient or normal-startup signals.
//
// Match order is significant: more specific matches (CreateContainerConfigError
// with a known message prefix, specific curl exit codes) come before the
// catch-all PackageFetchFailed bucket.
func classifyInitContainer(ics *corev1.ContainerStatus) (reason, detail string, isFailure bool) {
	if reason, detail, ok := classifyWaitingState(ics.State.Waiting); ok {
		return reason, detail, true
	}
	if reason, detail, ok := classifyTerminatedState(ics.State.Terminated); ok {
		return reason, detail, true
	}
	// CrashLoopBackOff: kubelet sets State.Waiting{reason=CrashLoopBackOff}
	// AND LastTerminationState.Terminated.ExitCode = the failing exit code.
	// classifyWaitingState above does not single out CrashLoopBackOff
	// (the message is uninformative); we use the last terminated exit code
	// for an accurate reason.
	if ics.RestartCount > 0 {
		if reason, detail, ok := classifyTerminatedState(ics.LastTerminationState.Terminated); ok {
			return reason, detail, true
		}
	}
	return "", "", false
}

// classifyWaitingState returns a non-empty failure classification only for
// waiting reasons that are definitive failures — kubelet's
// CreateContainerConfigError (with message-prefix discrimination per the
// Kind probe findings) and image-pull failures. PodInitializing is treated
// as transient and returns ok=false.
func classifyWaitingState(w *corev1.ContainerStateWaiting) (reason, detail string, ok bool) {
	if w == nil {
		return "", "", false
	}
	switch w.Reason {
	case "CreateContainerConfigError":
		switch {
		case strings.HasPrefix(w.Message, `secret "`):
			return v1alpha1.ReasonPackageSecretMissing, w.Message, true
		case strings.HasPrefix(w.Message, "couldn't find key"):
			return v1alpha1.ReasonPackageSecretKeyMissing, w.Message, true
		default:
			// Future kubelet message changes fall here so users still see
			// a reason + diagnostic detail instead of silence.
			return v1alpha1.ReasonPackageFetchFailed,
				fmt.Sprintf("CreateContainerConfigError: %s", w.Message), true
		}
	case "ImagePullBackOff", "ErrImagePull":
		return v1alpha1.ReasonPackageImagePullFailed,
			fmt.Sprintf("%s: %s", w.Reason, w.Message), true
	}
	return "", "", false
}

// classifyTerminatedState returns a non-empty failure classification only
// for non-zero exit codes. Specific exit codes (60=TLS verify, 22=HTTP,
// 58/82=client cert, 100=unzip) map to their dedicated reasons; the rest
// fall through to ReasonPackageFetchFailed with the exit code in the
// message.
//
// Exit code 51 (curl: "peer's SSL certificate not OK") is deliberately
// NOT in the explicit table because under the script's flags it can fire
// for either a server-side peer-cert problem (hostname mismatch, untrusted
// chain when only --cacert is set) or a client-cert problem. Routing it
// to a specific reason would mislead users who hit the wrong one; the
// catch-all surfaces the exit code so they can look up the cause.
func classifyTerminatedState(t *corev1.ContainerStateTerminated) (reason, detail string, ok bool) {
	if t == nil || t.ExitCode == 0 {
		return "", "", false
	}
	switch t.ExitCode {
	case exitCodeTLSVerifyFailed:
		return v1alpha1.ReasonPackageTLSVerifyFailed,
			fmt.Sprintf("exited %d (TLS verify failed)", t.ExitCode), true
	case exitCodeClientCertBad, exitCodeSSLEngineInit:
		return v1alpha1.ReasonPackageClientCertInvalid,
			fmt.Sprintf("exited %d (client cert / TLS engine error)", t.ExitCode), true
	case exitCodeHTTPError:
		return v1alpha1.ReasonPackageHTTPError,
			fmt.Sprintf("exited %d (HTTP 4xx/5xx)", t.ExitCode), true
	case exitCodeUnzipFailure:
		return v1alpha1.ReasonPackageInvalidArchive,
			fmt.Sprintf("exited %d (unzip failed; downloaded artifact is not a valid ZIP)", t.ExitCode), true
	default:
		return v1alpha1.ReasonPackageFetchFailed,
			fmt.Sprintf("exited %d", t.ExitCode), true
	}
}

// parsePackageFetchIndex extracts N from "package-fetch-N". The predicate
// only delivers names with this prefix; defensive parse so a malformed
// suffix degrades gracefully rather than panicking.
func parsePackageFetchIndex(containerName string) (int, bool) {
	suffix := strings.TrimPrefix(containerName, packageFetchContainerNamePrefix)
	if suffix == containerName || suffix == "" {
		return 0, false
	}
	var idx int
	if _, err := fmt.Sscanf(suffix, "%d", &idx); err != nil {
		return 0, false
	}
	return idx, true
}

// formatPodPackageMessage produces the translator's condition message,
// preferring the rich pre-check format (mountPath + URL) when the index
// resolves to a known PackageSpec entry, falling back to the index-only
// format when it does not (defensive: stale pod from a previous reconcile
// where the packages slice was longer, or unparseable container name).
// Keeps the pre-check leg and the pod-watch leg's messages identical when
// the spec is in sync, eliminating the original two-shape inconsistency.
func formatPodPackageMessage(idx int, packages []v1alpha1.PackageSpec, containerName, detail string) string {
	if idx >= 0 && idx < len(packages) {
		return formatPackageMessage(idx, packages[idx], detail)
	}
	return formatPackageMessageByIndex(idx, containerName, detail)
}

// formatPackageMessageByIndex is the fallback used when the translator
// cannot resolve a container's index back to a PackageSpec entry — e.g.,
// a stale pod whose package-fetch-N index is now out of range for the
// current spec, or a container with a malformed name suffix.
func formatPackageMessageByIndex(idx int, containerName, detail string) string {
	if idx >= 0 {
		return fmt.Sprintf("%s (index=%d): %s", containerName, idx, detail)
	}
	return fmt.Sprintf("%s: %s", containerName, detail)
}

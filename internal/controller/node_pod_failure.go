package controller

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// podFailureResult is the outcome of scanning a node's pods for a hard
// container failure. failing=false means no definitive failure was seen
// (leave the Degraded condition to computeNodeConditions).
type podFailureResult struct {
	failing bool
	message string
}

// translatePodFailure surfaces container failures (bad image, crashloop, config
// error) that computeNodeConditions can't see — a stuck rollout still reports
// Ready=True off the old pod. Scans the newest ReplicaSet's pods, returns the
// first definitive failure; package-fetch-* init containers are PackagesReady's.
func translatePodFailure(pods []corev1.Pod) podFailureResult {
	current := currentReplicaSetPods(pods)
	for i := range current {
		p := &current[i]
		for j := range p.Status.InitContainerStatuses {
			cs := &p.Status.InitContainerStatuses[j]
			if strings.HasPrefix(cs.Name, packageFetchContainerNamePrefix) {
				continue
			}
			if detail, ok := classifyContainerFailure(cs, true); ok {
				return podFailureResult{failing: true, message: fmt.Sprintf("init container %q: %s", cs.Name, detail)}
			}
		}
		for j := range p.Status.ContainerStatuses {
			cs := &p.Status.ContainerStatuses[j]
			if detail, ok := classifyContainerFailure(cs, false); ok {
				return podFailureResult{failing: true, message: fmt.Sprintf("container %q: %s", cs.Name, detail)}
			}
		}
	}
	return podFailureResult{}
}

// classifyContainerFailure returns a bounded human-readable cause when a
// container status is a definitive failure. Normal-startup states (ContainerCreating,
// PodInitializing, Running-not-yet-ready) return ok=false so the condition does
// not flap on transients. A non-zero Terminated is only treated as failure for
// init containers (which don't auto-restart); regular containers crash-loop via
// Waiting=CrashLoopBackOff instead.
func classifyContainerFailure(cs *corev1.ContainerStatus, isInit bool) (string, bool) {
	if w := cs.State.Waiting; w != nil {
		switch w.Reason {
		case "ImagePullBackOff", "ErrImagePull", "ErrImageNeverPull", "InvalidImageName", "RegistryUnavailable":
			// ErrImagePull and ImagePullBackOff alternate as kubelet retries; report
			// a normalized reason + the stable image ref (not kubelet's volatile
			// message) so the condition/event doesn't flap on each backoff cycle.
			return fmt.Sprintf("%s: cannot pull image %q", normalizeImagePullReason(w.Reason), cs.Image), true
		case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
			return joinReasonDetail(w.Reason, w.Message), true
		case "CrashLoopBackOff":
			if t := cs.LastTerminationState.Terminated; t != nil {
				if d := summarizeTerminationMessage(t.Message); d != "" {
					return fmt.Sprintf("CrashLoopBackOff (%s)", d), true
				}
				return fmt.Sprintf("CrashLoopBackOff (exit %d)", t.ExitCode), true
			}
			return "CrashLoopBackOff", true
		}
	}
	if isInit {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			if d := summarizeTerminationMessage(t.Message); d != "" {
				return fmt.Sprintf("exited %d (%s)", t.ExitCode, d), true
			}
			return fmt.Sprintf("exited %d (%s)", t.ExitCode, t.Reason), true
		}
	}
	return "", false
}

func joinReasonDetail(reason, msg string) string {
	if msg = strings.TrimSpace(msg); msg != "" {
		return fmt.Sprintf("%s (%s)", reason, truncateMessage(msg, 200))
	}
	return reason
}

// normalizeImagePullReason collapses kubelet's alternating ErrImagePull /
// ImagePullBackOff (the same failure mid-retry vs backing off) to one stable
// reason, so the failure signature and the surfaced message don't flap.
func normalizeImagePullReason(reason string) string {
	if reason == "ErrImagePull" {
		return "ImagePullBackOff"
	}
	return reason
}

// containerFailureStateDiffers reports whether the failure-relevant container
// state changed between two pod revisions — used by the pod-watch predicate to
// re-trigger reconciliation on a failure transition without waking on every
// heartbeat (resourceVersion/timestamp bumps leave the signature unchanged).
func containerFailureStateDiffers(oldPod, newPod *corev1.Pod) bool {
	return podFailureSignature(oldPod) != podFailureSignature(newPod)
}

// podFailureSignature records only the FAILING containers (per
// classifyContainerFailure) and their reason. It changes when a container
// enters or leaves a failure state — so the predicate fires on a failure
// transition and its recovery, but not on benign transitions (init success,
// ContainerCreating→Running) or heartbeat bumps. Messages are excluded so
// kubelet message wording churn doesn't wake the reconciler.
func podFailureSignature(pod *corev1.Pod) string {
	var b strings.Builder
	write := func(cs *corev1.ContainerStatus, isInit bool) {
		if _, ok := classifyContainerFailure(cs, isInit); !ok {
			return
		}
		b.WriteString(cs.Name)
		b.WriteByte('=')
		if w := cs.State.Waiting; w != nil {
			b.WriteString(normalizeImagePullReason(w.Reason))
		} else if t := cs.State.Terminated; t != nil {
			b.WriteString("exit" + strconv.Itoa(int(t.ExitCode)))
		}
		b.WriteByte(';')
	}
	for i := range pod.Status.InitContainerStatuses {
		write(&pod.Status.InitContainerStatuses[i], true)
	}
	b.WriteByte('#')
	for i := range pod.Status.ContainerStatuses {
		write(&pod.Status.ContainerStatuses[i], false)
	}
	return b.String()
}

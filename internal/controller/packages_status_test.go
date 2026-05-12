package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// testPackagesHash is the canonical packages-hash annotation value used
// across translator tests. Pods constructed via podOwnedBy carry it so
// the translator's hash filter does not reject them; tests that exercise
// the hash-mismatch case use podOwnedByWithHash to override.
const testPackagesHash = "test-hash"

// defaultTestPackages returns a small PackageSpec slice with two entries,
// covering the package-fetch-0 and package-fetch-1 indexes used by most
// translator tests. The mountPath / URL values let the translator's
// message-format path produce rich diagnostics.
func defaultTestPackages() []v1alpha1.PackageSpec {
	return []v1alpha1.PackageSpec{
		{MountPath: "/etc/plugins/zero", Source: v1alpha1.PackageSource{URL: "https://example.com/zero.zip"}},
		{MountPath: "/etc/plugins/one", Source: v1alpha1.PackageSource{URL: "https://example.com/one.zip"}},
	}
}

// podOwnedBy returns a Pod with a controller OwnerReference of kind
// ReplicaSet carrying the given UID, and the standard testPackagesHash
// annotation. CreationTimestamp ensures pods from "newer" ReplicaSets
// win when grouped — pass distinct values per test.
func podOwnedBy(name string, rsUID types.UID, created time.Time, initStatuses []corev1.ContainerStatus) corev1.Pod {
	return podOwnedByWithHash(name, rsUID, created, initStatuses, testPackagesHash)
}

// podOwnedByWithHash is podOwnedBy with an explicit packages-hash. Use to
// build pods that should be filtered out by the translator's hash gate.
func podOwnedByWithHash(name string, rsUID types.UID, created time.Time, initStatuses []corev1.ContainerStatus, hash string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ns",
			Annotations:       map[string]string{annotationPackagesHash: hash},
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       string(rsUID),
				UID:        rsUID,
				Controller: ptr.To(true),
			}},
		},
		Status: corev1.PodStatus{InitContainerStatuses: initStatuses},
	}
}

// Empty input → no decision.
func TestTranslatePackagesReady_NoPods(t *testing.T) {
	r := translatePackagesReady(nil, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("no pods must produce set=false; got %+v", r)
	}
}

// Pods exist but all are terminating → no decision (ignore old state).
func TestTranslatePackagesReady_AllPodsTerminating(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	now := metav1.Now()
	p.DeletionTimestamp = &now
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("terminating pods must be ignored; got %+v", r)
	}
}

// Pod is current but has no package-fetch containers in status yet.
// Brief window between scheduling and init-container start: omit, don't
// fabricate a True or False.
func TestTranslatePackagesReady_NoPackageFetchContainersYet(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		// only config-discovery (an unrelated init container) is reported
		{Name: "config-discovery", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("no package-fetch containers must produce set=false; got %+v", r)
	}
}

// All package-fetch containers exited 0 → True, AllPackagesFetched.
func TestTranslatePackagesReady_AllSucceeded(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
		terminatedInitStatus("package-fetch-1", 0),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.status != metav1.ConditionTrue || r.reason != v1alpha1.ReasonAllPackagesFetched {
		t.Errorf("all-success path: got %+v", r)
	}
	if !strings.Contains(r.message, "2 package-fetch") {
		t.Errorf("message must include success count: %q", r.message)
	}
}

// CreateContainerConfigError with `secret "X" not found` → PackageSecretMissing.
func TestTranslatePackagesReady_KubeletSecretMissing(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		waitingInitStatus("package-fetch-0", "CreateContainerConfigError", `secret "auth-creds" not found`),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.status != metav1.ConditionFalse || r.reason != v1alpha1.ReasonPackageSecretMissing {
		t.Errorf("expected SecretMissing False; got %+v", r)
	}
	if !strings.Contains(r.message, "auth-creds") {
		t.Errorf("message must echo the Secret name: %q", r.message)
	}
}

// CreateContainerConfigError with `couldn't find key ...` → PackageSecretKeyMissing.
func TestTranslatePackagesReady_KubeletKeyMissing(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		waitingInitStatus("package-fetch-1", "CreateContainerConfigError",
			"couldn't find key token in Secret ns/auth-creds"),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageSecretKeyMissing {
		t.Errorf("expected SecretKeyMissing; got %+v", r)
	}
	if !strings.Contains(r.message, "package-fetch-1") || !strings.Contains(r.message, "index=1") {
		t.Errorf("message must reference package-fetch-1 (index=1): %q", r.message)
	}
}

// CreateContainerConfigError with an unrecognized message — kubelet text
// changed in a future version, or a different config error. Must fall
// through to the open-set PackageFetchFailed reason with the verbatim
// message attached so diagnostics survive.
func TestTranslatePackagesReady_KubeletUnknownConfigError(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		waitingInitStatus("package-fetch-0", "CreateContainerConfigError", "some new kubelet message we don't know about"),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageFetchFailed {
		t.Errorf("expected open-set FetchFailed; got %+v", r)
	}
	if !strings.Contains(r.message, "some new kubelet message") {
		t.Errorf("message must include verbatim kubelet text: %q", r.message)
	}
}

// ImagePullBackOff / ErrImagePull → PackageImagePullFailed.
func TestTranslatePackagesReady_ImagePullBackOff(t *testing.T) {
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull"} {
		t.Run(reason, func(t *testing.T) {
			t0 := time.Now()
			p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
				waitingInitStatus("package-fetch-0", reason, "Back-off pulling image"),
			})
			r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
			if !r.set || r.reason != v1alpha1.ReasonPackageImagePullFailed {
				t.Errorf("waiting reason %q: got %+v", reason, r)
			}
		})
	}
}

// PodInitializing is a transient state — translator must NOT flip the
// condition based on it (would flap during normal pod startup).
func TestTranslatePackagesReady_PodInitializingIgnored(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		waitingInitStatus("package-fetch-0", "PodInitializing", ""),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("PodInitializing must produce set=false; got %+v", r)
	}
}

// Specific curl exit codes → their dedicated reasons.
func TestTranslatePackagesReady_CurlExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   int32
		wantReason string
	}{
		{"TLSVerifyFailed_60", 60, v1alpha1.ReasonPackageTLSVerifyFailed},
		{"HTTPError_22", 22, v1alpha1.ReasonPackageHTTPError},
		{"ClientCert_58", 58, v1alpha1.ReasonPackageClientCertInvalid},
		{"ClientCert_82", 82, v1alpha1.ReasonPackageClientCertInvalid},
		{"InvalidArchive_100", 100, v1alpha1.ReasonPackageInvalidArchive},
		// Exit 51 ("peer cert not OK") is ambiguous between server-side
		// and client-cert problems — falls to the open-set catch-all so
		// the exit code is visible in the message.
		{"CatchAll_PeerCert_51", 51, v1alpha1.ReasonPackageFetchFailed},
		{"CatchAll_DNS_6", 6, v1alpha1.ReasonPackageFetchFailed},
		{"CatchAll_Timeout_28", 28, v1alpha1.ReasonPackageFetchFailed},
		{"CatchAll_Size_63", 63, v1alpha1.ReasonPackageFetchFailed},
		{"CatchAll_OOM_137", 137, v1alpha1.ReasonPackageFetchFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t0 := time.Now()
			p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
				terminatedInitStatus("package-fetch-0", tc.exitCode),
			})
			r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
			if !r.set || r.status != metav1.ConditionFalse || r.reason != tc.wantReason {
				t.Errorf("exit %d: got %+v, want reason %q", tc.exitCode, r, tc.wantReason)
			}
			// Catch-all messages must include the exit code numerically.
			if tc.wantReason == v1alpha1.ReasonPackageFetchFailed {
				if !strings.Contains(r.message, "exited") {
					t.Errorf("catch-all message must include exit code: %q", r.message)
				}
			}
		})
	}
}

// CrashLoopBackOff: kubelet sets Waiting{reason=CrashLoopBackOff} AND
// LastTerminationState.Terminated.ExitCode=the failing code. Translator
// must consult lastState so users see the underlying failure, not the
// uninformative CrashLoopBackOff reason.
func TestTranslatePackagesReady_CrashLoopBackOffUsesLastState(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		{
			Name: "package-fetch-0",
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "Back-off restarting failed container"},
			},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 60},
			},
			RestartCount: 3,
		},
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageTLSVerifyFailed {
		t.Errorf("CrashLoopBackOff must use lastState exit code; got %+v", r)
	}
}

// Stale-pod ghost: an old ReplicaSet's failing pod coexists with a new
// ReplicaSet's healthy pod (the user just fixed their spec). The translator
// must pick the NEW ReplicaSet's pods and report success — not surface the
// doomed old pod's failure as the current state.
func TestTranslatePackagesReady_StalePodFromOldReplicaSetIgnored(t *testing.T) {
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()
	oldFail := podOwnedBy("old-failing", "rs-old", older, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	newOK := podOwnedBy("new-ok", "rs-new", newer, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
	})
	r := translatePackagesReady([]corev1.Pod{oldFail, newOK}, testPackagesHash, defaultTestPackages())
	if !r.set || r.status != metav1.ConditionTrue {
		t.Errorf("expected True from new ReplicaSet; got %+v", r)
	}
}

// Inverse of the previous: an old ReplicaSet's healthy pod still exists while
// a new ReplicaSet's pod is failing. Translator must report the new failure —
// the old success is stale.
func TestTranslatePackagesReady_StaleHealthyPodFromOldReplicaSetIgnored(t *testing.T) {
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()
	oldOK := podOwnedBy("old-ok", "rs-old", older, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
	})
	newFail := podOwnedBy("new-failing", "rs-new", newer, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	r := translatePackagesReady([]corev1.Pod{oldOK, newFail}, testPackagesHash, defaultTestPackages())
	if !r.set || r.status != metav1.ConditionFalse || r.reason != v1alpha1.ReasonPackageTLSVerifyFailed {
		t.Errorf("expected False (TLS verify) from new ReplicaSet; got %+v", r)
	}
}

// Two pods owned by the SAME ReplicaSet (e.g. 2 replicas, both crashlooping
// on the same broken spec). First failure wins.
func TestTranslatePackagesReady_MultipleReplicasFirstFailureWins(t *testing.T) {
	t0 := time.Now()
	p1 := podOwnedBy("rep-1", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	p2 := podOwnedBy("rep-2", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	r := translatePackagesReady([]corev1.Pod{p1, p2}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageTLSVerifyFailed {
		t.Errorf("expected TLSVerify False; got %+v", r)
	}
}

// One pod's first init container succeeds (package-fetch-0, exit 0), the
// second one fails. Sequential init means container 1 will not run after
// container 0's success — but kubelet reports both statuses. Translator
// must surface the FAILED one, not the earlier success.
func TestTranslatePackagesReady_LaterInitContainerFails(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
		terminatedInitStatus("package-fetch-1", 22),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageHTTPError {
		t.Errorf("expected HTTPError (package-fetch-1's exit=22); got %+v", r)
	}
	if !strings.Contains(r.message, "package-fetch-1") {
		t.Errorf("message must reference package-fetch-1: %q", r.message)
	}
}

// Pods owned by a non-ReplicaSet controller (e.g. a StatefulSet — defensive
// case for future workload types) are skipped — currentReplicaSetPods has
// no group for them.
func TestTranslatePackagesReady_NonReplicaSetControllerPodsIgnored(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	p.OwnerReferences[0].Kind = "StatefulSet"
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("non-ReplicaSet controllers must be ignored; got %+v", r)
	}
}

// Rolling-update window regression: pods carrying the OLD packages-hash
// (still serving the previous spec) must NOT contribute to the translator's
// decision. Without the hash filter, the operator would surface a stale
// PackagesReady=True on the new spec while the new ReplicaSet's pods
// haven't reported yet. (Plan: rolling-update Ready=True false-positive
// fix.)
func TestTranslatePackagesReady_OldHashPodsFilteredOut(t *testing.T) {
	t0 := time.Now()
	oldPod := podOwnedByWithHash("old", "rs-old", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0), // old pod succeeded
	}, "old-hash")
	r := translatePackagesReady([]corev1.Pod{oldPod}, "new-hash", defaultTestPackages())
	if r.set {
		t.Errorf("pod with stale packages-hash must NOT contribute to translator decision; "+
			"otherwise rolling-update window leaks PackagesReady=True for the new spec — got %+v", r)
	}
}

// Mixed-hash input: only pods carrying the EXPECTED hash should drive the
// decision. Here, an old pod (succeeded with old hash) coexists with a new
// pod (failed with new hash). Translator must surface the new pod's
// failure, not the old pod's success.
func TestTranslatePackagesReady_MixedHashOnlyExpectedCounts(t *testing.T) {
	t0 := time.Now()
	older := t0.Add(-10 * time.Minute)
	oldSuccess := podOwnedByWithHash("old", "rs-old", older, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
	}, "old-hash")
	newFail := podOwnedByWithHash("new", "rs-new", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	}, "new-hash")
	r := translatePackagesReady([]corev1.Pod{oldSuccess, newFail}, "new-hash", defaultTestPackages())
	if !r.set || r.status != metav1.ConditionFalse || r.reason != v1alpha1.ReasonPackageTLSVerifyFailed {
		t.Errorf("expected new-hash pod's TLS-verify failure to win; got %+v", r)
	}
}

// Empty expectedHash defensively returns no pods — packages-empty
// reconciles should never reach the translator, but if they do this
// short-circuits cleanly.
func TestTranslatePackagesReady_EmptyExpectedHashSkipsAll(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
	})
	r := translatePackagesReady([]corev1.Pod{p}, "", defaultTestPackages())
	if r.set {
		t.Errorf("empty expectedHash must short-circuit to set=false; got %+v", r)
	}
}

// The translator's message MUST include the failing package's mountPath
// and URL when the index resolves to a known PackageSpec — same shape as
// the pre-check leg, so users reading `kubectl describe` see consistent
// diagnostics regardless of which leg fired the condition.
func TestTranslatePackagesReady_RichMessageWithMountPathAndURL(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-1", 22),
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageHTTPError {
		t.Fatalf("expected HTTPError; got %+v", r)
	}
	for _, want := range []string{
		"package-fetch-1",
		"index=1",
		"mountPath=/etc/plugins/one", // from defaultTestPackages[1]
		"url=https://example.com/one.zip",
		"exited 22",
	} {
		if !strings.Contains(r.message, want) {
			t.Errorf("translator message missing %q: %q", want, r.message)
		}
	}
}

// Defensive: if the container index is out of range for the current
// packages slice (e.g. a stale pod from a previous reconcile where the
// slice was longer), the message falls back to the index-only format
// instead of panicking.
func TestTranslatePackagesReady_RichMessageOutOfRangeFallback(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-7", 60), // index 7 doesn't exist in defaultTestPackages (len=2)
	})
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if !r.set || r.reason != v1alpha1.ReasonPackageTLSVerifyFailed {
		t.Fatalf("expected TLSVerifyFailed; got %+v", r)
	}
	if !strings.Contains(r.message, "package-fetch-7") || !strings.Contains(r.message, "index=7") {
		t.Errorf("out-of-range message must still include container name and index: %q", r.message)
	}
	if strings.Contains(r.message, "mountPath=") || strings.Contains(r.message, "url=") {
		t.Errorf("out-of-range index must NOT include mountPath/url (no spec to look up): %q", r.message)
	}
}

// Pod with no packages-hash annotation at all is filtered out (defensive:
// a pod that predates the annotation, or a hand-crafted pod, should not
// hijack the decision).
func TestTranslatePackagesReady_MissingAnnotationFilteredOut(t *testing.T) {
	t0 := time.Now()
	p := podOwnedBy("p", "rs-1", t0, []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
	})
	delete(p.Annotations, annotationPackagesHash)
	r := translatePackagesReady([]corev1.Pod{p}, testPackagesHash, defaultTestPackages())
	if r.set {
		t.Errorf("pod missing the packages-hash annotation must be filtered out; got %+v", r)
	}
}

// parsePackageFetchIndex tests — covers happy path and defensive degradation.
func TestParsePackageFetchIndex(t *testing.T) {
	cases := []struct {
		name   string
		want   int
		wantOK bool
	}{
		{"package-fetch-0", 0, true},
		{"package-fetch-7", 7, true},
		{"package-fetch-99", 99, true},
		{"package-fetch-", 0, false},
		{"package-fetch-abc", 0, false},
		{"config-discovery", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePackageFetchIndex(tc.name)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

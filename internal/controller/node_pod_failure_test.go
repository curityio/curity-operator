package controller

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

func waitingStatus(name, reason, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg},
	}}
}

func terminatedStatus(name string, exit int32, msg string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: msg, Reason: "Error"},
	}}
}

func rsPod(name, rsUID string, created metav1.Time, init, regular []corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: created,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", UID: types.UID(rsUID), Controller: ptr.To(true)},
			},
		},
		Status: corev1.PodStatus{InitContainerStatuses: init, ContainerStatuses: regular},
	}
}

func TestClassifyContainerFailure(t *testing.T) {
	cases := []struct {
		name   string
		cs     corev1.ContainerStatus
		isInit bool
		fail   bool
		substr string
	}{
		{"image pull backoff", waitingStatus("c", "ImagePullBackOff", `Back-off pulling image "x"`), false, true, "ImagePullBackOff"},
		{"err image never pull", waitingStatus("c", "ErrImageNeverPull", ""), false, true, "ErrImageNeverPull"},
		{"config error", waitingStatus("c", "CreateContainerConfigError", "secret missing"), false, true, "CreateContainerConfigError"},
		{"pod initializing not a failure", waitingStatus("c", "PodInitializing", ""), false, false, ""},
		{"running not a failure", corev1.ContainerStatus{Name: "c", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, false, false, ""},
		{"init terminated nonzero", terminatedStatus("c", 1, "boom"), true, true, "exited 1"},
		{"regular terminated nonzero is transient (not flagged)", terminatedStatus("c", 1, "boom"), false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, ok := classifyContainerFailure(&tc.cs, tc.isInit)
			if ok != tc.fail {
				t.Fatalf("isFailure: got %v want %v (detail=%q)", ok, tc.fail, detail)
			}
			if tc.fail && !strings.Contains(detail, tc.substr) {
				t.Errorf("detail %q missing %q", detail, tc.substr)
			}
		})
	}
}

func TestClassifyContainerFailure_CrashLoopUsesLastTerminated(t *testing.T) {
	cs := waitingStatus("c", "CrashLoopBackOff", "")
	cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ExitCode: 137}
	detail, ok := classifyContainerFailure(&cs, false)
	if !ok || !strings.Contains(detail, "CrashLoopBackOff") {
		t.Fatalf("want CrashLoopBackOff failure, got ok=%v detail=%q", ok, detail)
	}
}

func TestTranslatePodFailure(t *testing.T) {
	now := metav1.Now()
	t.Run("user init image pull is surfaced", func(t *testing.T) {
		pods := []corev1.Pod{rsPod("p", "rs1", now,
			[]corev1.ContainerStatus{waitingStatus("warmup", "ImagePullBackOff", "bad image")}, nil)}
		res := translatePodFailure(pods)
		if !res.failing || !strings.Contains(res.message, `init container "warmup"`) {
			t.Fatalf("want warmup failure, got %+v", res)
		}
	})

	t.Run("package-fetch container is skipped (PackagesReady owns it)", func(t *testing.T) {
		pods := []corev1.Pod{rsPod("p", "rs1", now,
			[]corev1.ContainerStatus{waitingStatus("package-fetch-0", "ImagePullBackOff", "")}, nil)}
		if res := translatePodFailure(pods); res.failing {
			t.Errorf("package-fetch failure must not surface on Degraded: %+v", res)
		}
	})

	t.Run("healthy pods report no failure", func(t *testing.T) {
		pods := []corev1.Pod{rsPod("p", "rs1", now, nil,
			[]corev1.ContainerStatus{{Name: "curity", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}})}
		if res := translatePodFailure(pods); res.failing {
			t.Errorf("healthy pod flagged: %+v", res)
		}
	})

	t.Run("stuck new rollout surfaces despite old pod healthy", func(t *testing.T) {
		older := metav1.NewTime(now.Add(-time.Hour))
		oldPod := rsPod("old", "rs-old", older, nil,
			[]corev1.ContainerStatus{{Name: "curity", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}})
		newPod := rsPod("new", "rs-new", now,
			[]corev1.ContainerStatus{waitingStatus("warmup", "ImagePullBackOff", "bad")}, nil)
		res := translatePodFailure([]corev1.Pod{oldPod, newPod})
		if !res.failing || !strings.Contains(res.message, "warmup") {
			t.Fatalf("newest RS failure must win: %+v", res)
		}
	})
}

func TestContainerFailureStateDiffers(t *testing.T) {
	base := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "curity", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
	}}}
	// Heartbeat: identical container states → no re-trigger.
	if containerFailureStateDiffers(&base, base.DeepCopy()) {
		t.Errorf("identical states should not differ")
	}
	// Transition into a failure → re-trigger.
	failed := base.DeepCopy()
	failed.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
	if !containerFailureStateDiffers(&base, failed) {
		t.Errorf("ContainerCreating→ImagePullBackOff should differ")
	}
}

func TestContainerFailureStateDiffers_ImagePullPairDoesNotFlap(t *testing.T) {
	// kubelet alternates ErrImagePull ↔ ImagePullBackOff while retrying; the
	// normalized signature must treat them as the same failure (no re-trigger).
	errPull := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "c", Image: "x:1", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
	}}}
	backoff := errPull.DeepCopy()
	backoff.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
	if containerFailureStateDiffers(&errPull, backoff) {
		t.Error("ErrImagePull↔ImagePullBackOff must not re-trigger (same failure)")
	}
}

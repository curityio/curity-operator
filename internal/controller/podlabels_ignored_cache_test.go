package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestPodLabelsIgnoredCache_ShouldWarn(t *testing.T) {
	var c podLabelsIgnoredCache
	const uid, other = "uid-1", "uid-2"
	steps := []struct {
		name string
		uid  string
		gen  int64
		keys string
		want bool
	}{
		{"no collision never warns", uid, 1, "", false},
		{"first collision warns", uid, 1, "app.kubernetes.io/version", true},
		{"same generation and set is deduped (stale re-reconcile)", uid, 1, "app.kubernetes.io/version", false},
		{"set change without a node edit warns (cluster podLabels edit)", uid, 1, "app.kubernetes.io/instance,app.kubernetes.io/version", true},
		{"node spec edit warns again", uid, 2, "app.kubernetes.io/instance,app.kubernetes.io/version", true},
		{"other node is tracked separately", other, 2, "app.kubernetes.io/instance,app.kubernetes.io/version", true},
		{"collision removed", uid, 2, "", false},
		{"collision re-added warns", uid, 2, "app.kubernetes.io/instance,app.kubernetes.io/version", true},
	}
	for _, s := range steps {
		if got := c.shouldWarn(types.UID(s.uid), s.gen, s.keys); got != s.want {
			t.Fatalf("%s: shouldWarn = %v; want %v", s.name, got, s.want)
		}
	}

	c.forget(types.UID(uid))
	if !c.shouldWarn(types.UID(uid), 2, "app.kubernetes.io/version") {
		t.Fatal("after forget: a recreated node must warn")
	}
}

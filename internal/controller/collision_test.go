package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func newCollisionNode(creation time.Time, uid string) *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(creation),
			UID:               types.UID(uid),
		},
	}
}

func TestNodeIsNewer(t *testing.T) {
	t0 := time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Second)

	cases := []struct {
		name string
		a    *v1alpha1.IdentityServerNode
		b    *v1alpha1.IdentityServerNode
		want bool
	}{
		{
			name: "a created after b",
			a:    newCollisionNode(t1, "uid-a"),
			b:    newCollisionNode(t0, "uid-b"),
			want: true,
		},
		{
			name: "a created before b",
			a:    newCollisionNode(t0, "uid-a"),
			b:    newCollisionNode(t1, "uid-b"),
			want: false,
		},
		{
			name: "same timestamp, a has larger UID",
			a:    newCollisionNode(t0, "uid-b"),
			b:    newCollisionNode(t0, "uid-a"),
			want: true,
		},
		{
			name: "same timestamp, a has smaller UID",
			a:    newCollisionNode(t0, "uid-a"),
			b:    newCollisionNode(t0, "uid-b"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeIsNewer(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("nodeIsNewer() = %v, want %v", got, tc.want)
			}
			// Symmetry: exactly one side is "newer" when UIDs differ.
			reverse := nodeIsNewer(tc.b, tc.a)
			if reverse == got {
				t.Errorf("nodeIsNewer is not antisymmetric: forward=%v reverse=%v", got, reverse)
			}
		})
	}
}

package main

import (
	"reflect"
	"testing"
)

func TestParseWatchNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "empty watches all", raw: "", want: nil},
		{name: "only separators and spaces", raw: " , ,", want: nil},
		{name: "single namespace", raw: "team-a", want: []string{"team-a"}},
		{name: "list with spaces", raw: " team-a, team-b ,team-c", want: []string{"team-a", "team-b", "team-c"}},
		{name: "duplicates removed, order kept", raw: "team-b,team-a,team-b", want: []string{"team-b", "team-a"}},
		{name: "uppercase rejected", raw: "Team-A", wantErr: true},
		{name: "dots rejected", raw: "team.a", wantErr: true},
		{name: "one bad entry fails the list", raw: "team-a,-bad", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWatchNamespaces(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

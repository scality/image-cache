package controller

import (
	"maps"
	"testing"
)

const (
	osLabelKey   = "kubernetes.io/os"
	osLabelLinux = "linux"
	roleLabelKey = "role"
	roleWorker   = "worker"
	// zoneLabelKey is a label key no suite CR selector ever uses, so tests
	// use it to build nodes/selectors that deliberately do not match.
	zoneLabelKey = "zone"
	// workerResourceName is an example ImageCache resource name, reused
	// across this file and the node reconciler tests.
	workerResourceName = "worker-134-0-0"
	// emptyValLabelKey is a label key present on the node with an empty
	// value, used to distinguish "present but empty" from "absent".
	emptyValLabelKey = "emptyval"
	// missingLabelKey is a label key absent from the node's labels.
	missingLabelKey = "newkey"
)

func TestMatches(t *testing.T) {
	node := map[string]string{osLabelKey: osLabelLinux, roleLabelKey: roleWorker, emptyValLabelKey: ""}
	cases := []struct {
		name     string
		selector map[string]string
		want     bool
	}{
		{"empty selects all", nil, true},
		{"subset matches", map[string]string{roleLabelKey: roleWorker}, true},
		{"value mismatch", map[string]string{roleLabelKey: "master"}, false},
		{"missing key", map[string]string{zoneLabelKey: "a"}, false},
		{"empty value vs absent label", map[string]string{missingLabelKey: ""}, false},
		{"empty value vs present empty label", map[string]string{emptyValLabelKey: ""}, true},
	}
	for _, c := range cases {
		if got := matches(c.selector, node); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestApplyStatusLabels(t *testing.T) {
	current := map[string]string{
		osLabelKey:                       osLabelLinux,
		LabelPrefix + "worker-133-0-0":   StatusSynced,
		LabelPrefix + workerResourceName: StatusPending,
	}
	got, changed := applyStatusLabels(current, map[string]string{workerResourceName: StatusSynced})
	if !changed {
		t.Fatal("want changed")
	}
	want := map[string]string{
		osLabelKey:                       osLabelLinux,
		LabelPrefix + workerResourceName: StatusSynced,
	}
	if !maps.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if _, changed := applyStatusLabels(got, map[string]string{workerResourceName: StatusSynced}); changed {
		t.Error("idempotent call must report no change")
	}
}

func TestApplyStatusLabelsNilInput(t *testing.T) {
	got, changed := applyStatusLabels(nil, map[string]string{"a": StatusPending})
	if !changed || got[LabelPrefix+"a"] != StatusPending {
		t.Errorf("nil labels: got %v changed=%v", got, changed)
	}
}

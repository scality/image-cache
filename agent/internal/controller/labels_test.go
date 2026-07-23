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
		{"missing key", map[string]string{"zone": "a"}, false},
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
		osLabelKey:                     osLabelLinux,
		LabelPrefix + "worker-133-0-0": StatusSynced,
		LabelPrefix + "worker-134-0-0": StatusPending,
	}
	got, changed := applyStatusLabels(current, map[string]string{"worker-134-0-0": StatusSynced})
	if !changed {
		t.Fatal("want changed")
	}
	want := map[string]string{
		osLabelKey:                     osLabelLinux,
		LabelPrefix + "worker-134-0-0": StatusSynced,
	}
	if !maps.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if _, changed := applyStatusLabels(got, map[string]string{"worker-134-0-0": StatusSynced}); changed {
		t.Error("idempotent call must report no change")
	}
}

func TestApplyStatusLabelsNilInput(t *testing.T) {
	got, changed := applyStatusLabels(nil, map[string]string{"a": StatusPending})
	if !changed || got[LabelPrefix+"a"] != StatusPending {
		t.Errorf("nil labels: got %v changed=%v", got, changed)
	}
}

package controller

import (
	"maps"
	"strings"
)

// LabelPrefix prefixes the per-resource sync-status labels set on nodes.
const LabelPrefix = "image-cache.scality.com/"

// Sync-status label values.
const (
	StatusSynced  = "synced"
	StatusPending = "pending"
)

// matches reports whether every selector entry equals the node label value.
// An empty selector matches every node (pod nodeSelector semantics).
func matches(selector, nodeLabels map[string]string) bool {
	for k, v := range selector {
		if actual, ok := nodeLabels[k]; !ok || actual != v {
			return false
		}
	}
	return true
}

// applyStatusLabels returns a copy of labels where the LabelPrefix-prefixed
// entries are exactly want (keyed without the prefix), plus whether anything
// changed. Labels outside the prefix are preserved untouched.
func applyStatusLabels(labels map[string]string, want map[string]string) (map[string]string, bool) {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	changed := false
	for k := range out {
		if !strings.HasPrefix(k, LabelPrefix) {
			continue
		}
		if _, ok := want[strings.TrimPrefix(k, LabelPrefix)]; !ok {
			delete(out, k)
			changed = true
		}
	}
	for k, v := range want {
		if out[LabelPrefix+k] != v {
			out[LabelPrefix+k] = v
			changed = true
		}
	}
	return out, changed
}

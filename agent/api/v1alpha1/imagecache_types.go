/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ImageCacheSpec defines the desired state of ImageCache.
type ImageCacheSpec struct {
	// nodeSelector selects the nodes this cache applies to, by exact
	// key/value match against node labels (same semantics as a pod's
	// .spec.nodeSelector). Empty or absent selects every node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// source is the reference of the image whose layers contain the
	// tarballs to cache. The reference uses the usual
	// registry/repository[:tag|@digest] form; the registry may carry a port,
	// the repository is lowercase, and a digest is sha256.
	// The length bound is what keeps the CEL rule below the API server's
	// per-rule cost budget, which is computed from the declared maximum.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:XValidation:rule="self.matches('^([a-zA-Z0-9][a-zA-Z0-9.-]*(:[0-9]+)?/)?[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*(:[a-zA-Z0-9_][a-zA-Z0-9._-]*)?(@sha256:[a-f0-9]{64})?$')",message="source must be a container image reference, of the form registry[:port]/repository[:tag][@sha256:<digest>]"
	// +required
	Source string `json:"source"`

	// cachePath is the host directory under which the tarballs are
	// extracted, in a subdirectory named after this resource. It must
	// start with '/' and must not contain the substring '..' anywhere
	// (a plain substring check, not path-segment parsing).
	// +kubebuilder:default=/var/lib/image-cache
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="cachePath must not contain '..'"
	// +optional
	CachePath string `json:"cachePath,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="CachePath",type=string,JSONPath=`.spec.cachePath`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must not exceed 63 characters: it is used as a node label name"

// ImageCache declares desired boot-cache content for a set of nodes. The
// agent reports per-node sync status through node labels
// (image-cache.scality.com/<name>: synced|pending), so the resource name
// should be chosen up front: metadata.generateName is discouraged.
type ImageCache struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ImageCache
	// +required
	Spec ImageCacheSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// ImageCacheList contains a list of ImageCache
type ImageCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ImageCache `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ImageCache{}, &ImageCacheList{})
		return nil
	})
}

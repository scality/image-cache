package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagecachev1alpha1 "github.com/scality/image-cache/agent/api/v1alpha1"
)

// The API server, not the agent, is what rejects a malformed spec: these
// specs are submitted to a real one (envtest) and never reach a reconciler.
var _ = Describe("ImageCache validation", func() {
	create := func(name, source string) error {
		ic := &imagecachev1alpha1.ImageCache{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       imagecachev1alpha1.ImageCacheSpec{Source: source},
		}
		err := k8sClient.Create(ctx, ic)
		if err == nil {
			Expect(k8sClient.Delete(ctx, ic)).To(Succeed())
		}
		return err
	}

	DescribeTable("accepts the reference forms a registry serves",
		func(source string) {
			Expect(create("valid-source", source)).To(Succeed())
		},
		Entry("bare repository", "nginx"),
		Entry("repository and tag", "library/nginx:1.29.4"),
		Entry("registry, repository and tag", "ghcr.io/scality/file-reflector:v0.2.0"),
		Entry("registry with a port", "registry.example.com:5000/boot-cache/worker:134.0.0"),
		Entry("separators inside a path component", "example.com/some_team/boot-cache.v2:latest"),
		Entry("digest", "example.com/boot-cache@sha256:"+
			"97f14f857ebef1b421ad2799e6fba77c9b0cebcfc19f0b726344e322a4f7ead5"),
	)

	DescribeTable("rejects what a registry could not resolve",
		func(source string) {
			Expect(create("invalid-source", source)).NotTo(Succeed())
		},
		Entry("empty", ""),
		Entry("whitespace", "example.com/boot cache:1.0.0"),
		Entry("uppercase repository", "example.com/Boot-Cache:1.0.0"),
		Entry("trailing colon", "example.com/boot-cache:"),
		Entry("a path, not a reference", "/var/lib/image-cache/worker.tar"),
		Entry("truncated digest", "example.com/boot-cache@sha256:97f14f85"),
		Entry("digest that is not hexadecimal", "example.com/boot-cache@sha256:"+
			"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"),
	)
})

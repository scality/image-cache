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
	"fmt"
	"os"
	"strings"
	"testing"
)

const crdPath = "../../config/crd/bases/image-cache.scality.com_imagecaches.yaml"

// See the constants in imagecache_types.go for why this test exists. A drift
// would otherwise only show on a node: the command would reject a name the
// API server accepts, or write a directory under a name no resource can carry.
func TestGeneratedCRDMatchesTheExportedLimits(t *testing.T) {
	crd, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("reading the generated CRD: %v", err)
	}
	for _, want := range []string{
		fmt.Sprintf("size(self.metadata.name) <= %d", ResourceNameMax),
		fmt.Sprintf("!self.contains(''%s'')", CachePathParent),
	} {
		if !strings.Contains(string(crd), want) {
			t.Errorf("the generated CRD no longer carries %q; run `make manifests` "+
				"or bring the constant back in line with the marker", want)
		}
	}
}

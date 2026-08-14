# image-cache-agent

> This module lives at `agent/` inside the image-cache monorepo. See
> `DESIGN.md` next to this file for the actual design; proper documentation
> is tracked separately and will replace this scaffold README.

A DaemonSet that keeps the container image cache of each node in sync with
the `ImageCache` resources selecting it: it pulls the declared images,
extracts the tarballs they carry into the cache directory, garbage-collects
what is no longer declared, and reports per-node progress as node labels.

## Getting Started

### Prerequisites
- go version v1.26+
- docker version 17.03+.
- kubectl and access to a Kubernetes cluster supporting CEL CRD validation
  (v1.25+).

All the commands below are run from this directory (`agent/`).

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/image-cache-agent:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/image-cache-agent:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Contributing

See `DESIGN.md` for the architecture and the Testing section for how the
test suites are organized (`make test`, `make test-e2e`, `make lint`).

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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


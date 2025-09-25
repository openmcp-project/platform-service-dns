# DNS Service Configuration

The _PlatformService DNS_ requires configuration in form of a custom resource that is registered during the `init` step of the operator.
The `DNSServiceConfig` resource is cluster-scoped and must have the same name as the PlatformService it is meant for.

```yaml
apiVersion: dns.openmcp.cloud/v1alpha1
kind: DNSServiceConfig
metadata:
  name: dns
  namespace: openmcp-system
spec:
  externalDNSSource:
    chartName: charts/external-dns # path to the external-dns helm chart within the chosen repository
    git:
      url: https://github.com/kubernetes-sigs/external-dns
      interval: 1h
      ref:
        tag: v0.18.0

  externalDNSForPurposes:
  - name: my-identifier # optional, for logging purposes only
    purposeSelector:
      and:
      - or:
        - name: foo
        - name: bar
      - name: asdf
    helmValues:
      foo: bar
      asdf: qwer
```

#### Helm Chart Source

`spec.externalDNSSource` is required and describes where to find the helm chart for the [external-dns](https://github.com/kubernetes-sigs/external-dns) controller. Next to `chartName`, it has to contain exactly one of either `git`, `helm` or `oci`. The content of this field will then be used as `spec` for a Flux [`GitRepository`](https://fluxcd.io/flux/components/source/gitrepositories/), [`HelmRepository`](https://fluxcd.io/flux/components/source/helmrepositories/), or [`OCIRepository`](https://fluxcd.io/flux/components/source/ocirepositories/), respectively.

While the `HelmRepository` spec already contains a reference to a branch or tag, for `helm` and `oci`, the desired version can be specified by appending it to `chartName` with an `@` as separator, e.g. `chartName: charts/external-dns@v1.2.3`. The version is otherwise assumed to be `latest`.

#### Purpose Mapping

`spec.externalDNSForPurposes` maps purpose selectors to helm values for the external-dns deployment. Its `name` is optional and just used for better log and error messages.

`helmValues` contains the values to be passed into the helm release. They are simply forwarded, but a few keywords are replaced with specific values:
- `<provider.name>` resolves to the name of the `PlatformService`.
- `<provider.namespace>` resolves to the namespace the operator pod is running in.
- `<environment>` resolves to the PlatformService's environment.
- `<cluster.name>` resolves to the name of the `Cluster` resource the deployment belongs to.
- `<cluster.namespace>` resolves to the namespace of the `Cluster` resource the deployment belongs to.

The `purposeSelector` defines which `Cluster` resources should get the `external-dns` deployment. Because a `Cluster` can have multiple purposes, a simple mapping from a purpose to the configuration would not suffice. Therefore, the `purposeSelector` field is a recursive struct that allows to specify complex purpose selectors. Exactly one of the allowed fields `name`, `and`, `or`, and `not` must be set:
- The `name` field takes a string. If set, the selector matches if the shoot purposes contain the purpose with the given name.
- `not` takes a purpose selector and negates its result.
- `and` takes a list of purpose selectors and matches only if all of them match.
- Similarly, `or` also takes a list of purpose selectors, but matches if at least one of them matches.
- An empty purpose selector always matches.

> It is not validated that only one of the mentioned fields is set. If multiple ones are set, only one of them will be evaluated and the rest will be ignored.

Whenever a `Cluster` is reconciled, the selectors are applied in the order they are specified in. The first mapping where the selector matches decides the configuration with which `external-dns` is deployed. If no selector matches, `external-dns` is not deployed on the respective `Cluster`.

##### Examples

Here are a few examples for purpose selectors and what they match:

###### Example 1
```yaml
purposeSelector:
  name: foo
```
Probably the most common use cases: Matches every `Cluster` where `spec.purposes` contains `foo`.

###### Example 2
```yaml
purposeSelector: {}
```
Matches all `Cluster` resources.

###### Example 3
```yaml
purposeSelector:
  and:
  - or:
    - name: foo
    - name: bar
  - not:
      and:
      - name: foo
      - name: bar
```
Basically an `XOR`, matches all `Cluster`s that have either `foo` or `bar` among their purposes, but not both of them.

###### Example 4
```yaml
purposeSelector:
  not:
    name: foo
```
Matches all `Cluster` resources that do not have `foo` in their purpose list.

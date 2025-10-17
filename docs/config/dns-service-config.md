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
  secretsToCopy: # optional
    toPlatformCluster: # optional
    - source:
        name: my-secret
      target: # optional
        name: my-copied-secret
    toTargetCluster: [] # optional

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

#### Secret Copying

The `secretsToCopy` field allows to specify secrets that should be copied (in addition to the image pull secrets from the `PlatformService` resource). The source of the secrets is always the provider namespace on the platform cluster.

Secrets referenced in `secretsToCopy.toPlatformCluster` will be copied into the reconciled `Cluster` resource's namespace on the platform cluster. This is the namespace that will host the Flux source resource and where pull secrets for the helm chart have to reside.

Secrets referenced in `secretsToCopy.toTargetCluster` will be copied into the namespace where the helm chart will be deployed into on the cluster represented by the reconciled `Cluster` resource. This is useful if secrets are referenced in the deployed chart's values.

In both cases, if the entry's `target` field is set, the secret will be renamed to that name when copied, otherwise it will keep its source name.

If a secret that is to be created by the copy mechanism already exists, but is not managed by this controller (identified via labels), this will result in an error.

⚠️ Note that the secrets referenced in `spec.imagePullSecrets` of the `PlatformService` resource will always be copied to both, platform cluster and target cluster.

⚠️ **Warning: This mechanism can copy secrets to other namespaces and even other clusters, therefore potentially making them accessible to users which do not have permissions to access the source secret. Use with caution!**

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

### Configuration Examples

All examples below use a purpose selector that matches all `Cluster` resources which have `test` among their purposes.

###### Example 1 - Git Repo

```yaml
apiVersion: dns.openmcp.cloud/v1alpha1
kind: DNSServiceConfig
metadata:
  name: dns
spec:
  secretsToCopy:
    toTargetCluster:
    - source:
        name: route53-access

  externalDNSSource:
    chartName: charts/external-dns
    git:
      url: https://github.com/kubernetes-sigs/external-dns
      interval: 1h
      ref:
        tag: v0.19.0

  externalDNSForPurposes:
  - name: test
    purposeSelector:
      name: test
    helmValues:
      policy: sync
      txtOwnerId: '<environment>.<cluster.namespace>.<cluster.name>'
      sources:
      - service
      - gateway-httproute
      - gateway-tlsroute
      provider:
        name: aws
      env:
      - name: AWS_DEFAULT_REGION
        value: eu-central-1
      - name: AWS_SHARED_CREDENTIALS_FILE
        value: /.aws/credentials
      extraVolumes:
      - name: aws-credentials
        secret:
          secretName: route53-access
      extraVolumeMounts:
      - name: aws-credentials
        mountPath: /.aws
        readOnly: true
```

The AWS secret for this example is expected to look like this:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: route53-access
  namespace: openmcp-system
stringData:
  credentials: |
    [default]
    aws_access_key_id=<access-key-id>
    aws_secret_access_key=<secret-access-key>
type: Opaque
```

###### Example 2 - OCI Repo with Auth Secret

```yaml
apiVersion: dns.openmcp.cloud/v1alpha1
kind: DNSServiceConfig
metadata:
  name: dns
  namespace: openmcp-system
spec:
  secretsToCopy:
    toTargetCluster:
    - source:
        name: route53-access
    toPlatformCluster:
    - source:
        name: ghcr-access # pull secret for OCI registry holding the helm chart

  externalDNSSource:
    oci:
      url: oci://ghcr.io/my-user/external-dns
      interval: 1h
      ref:
        tag: "1.19.0"
      secretRef:
        name: ghcr-access

  externalDNSForPurposes:
  # similar to example 1
```

###### Example 3 - Helm Repo

```yaml
apiVersion: dns.openmcp.cloud/v1alpha1
kind: DNSServiceConfig
metadata:
  name: dns
  namespace: openmcp-system
spec:
  secretsToCopy:
    toTargetCluster:
    - source:
        name: route53-access

  externalDNSSource:
    chartName: external-dns@1.19.0
    helm:
      url: https://kubernetes-sigs.github.io/external-dns/
      interval: 1h

  externalDNSForPurposes:
  # similar to example 1
```

[![REUSE status](https://api.reuse.software/badge/github.com/openmcp-project/platform-service-dns)](https://api.reuse.software/info/github.com/openmcp-project/platform-service-dns)

# platform-service-dns

## About this project

PlatformService DNS is a `PlatformService` as described in [the OpenMCP Architecture Docs](https://github.com/openmcp-project/docs/blob/main/architecture/general/open-mcp-landscape-overview.md). 

It is a k8s controller that reconciles `Cluster` resources (from our [Cluster API](https://github.com/openmcp-project/docs/blob/main/adrs/cluster-api.md)) and deploys the [external-dns](https://github.com/kubernetes-sigs/external-dns) operator with a configuration depending on the `Cluster`'s purpose(s). The main goal of the service is to help setting up cross-cluster DNS routing for dynamically managed clusters, especially to enable `ValidatingWebhookConfiguration` resources pointing to webhooks served on other clusters.

## Requirements and Setup

In combination with the [openMCP Operator](https://github.com/openmcp-project/openmcp-operator), this controller can be deployed via a simple k8s resource:
```yaml
apiVersion: openmcp.cloud/v1alpha1
kind: PlatformService
metadata:
  name: dns
spec:
  image: "ghcr.io/openmcp-project/images/platform-service-dns:v0.1.0"
```

To run it locally, run
```shell
go run ./cmd/platform-service-dns/main.go init --environment default --provider-name dns --kubeconfig path/to/kubeconfig
```
to deploy the CRDs that are required for the controller and then
```shell
go run ./cmd/platform-service-dns/main.go run --environment default --provider-name dns --kubeconfig path/to/kubeconfig
```

Note that a `DNSServiceConfig` resources is required for the platform service. See the [documentation](docs/README.md) for further details regarding resources and configuration.

## Support, Feedback, Contributing

This project is open to feature requests/suggestions, bug reports etc. via [GitHub issues](https://github.com/openmcp-project/platform-service-dns/issues). Contribution and feedback are encouraged and always welcome. For more information about how to contribute, the project structure, as well as additional contribution information, see our [Contribution Guidelines](CONTRIBUTING.md).

## Security / Disclosure
If you find any bug that may be a security problem, please follow our instructions at [in our security policy](https://github.com/openmcp-project/platform-service-dns/security/policy) on how to report it. Please do not create GitHub issues for security-related doubts or problems.

## Code of Conduct

We as members, contributors, and leaders pledge to make participation in our community a harassment-free experience for everyone. By participating in this project, you agree to abide by its [Code of Conduct](https://github.com/SAP/.github/blob/main/CODE_OF_CONDUCT.md) at all times.

## Licensing

Copyright 2025 SAP SE or an SAP affiliate company and platform-service-dns contributors. Please see our [LICENSE](LICENSE) for copyright and license information. Detailed information including third-party components and their licensing/copyright information is available [via the REUSE tool](https://api.reuse.software/info/github.com/openmcp-project/platform-service-dns).

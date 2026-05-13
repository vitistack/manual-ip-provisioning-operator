# manual-ip-provisioning-operator

A Kubernetes operator that handles **manual network provisioning** for `NetworkNamespace` resources in the vitistack platform.

## Overview

In the vitistack architecture, a `NetworkNamespace` represents a network segment that needs to be provisioned before workloads can be scheduled. There are two provisioning modes:

| Mode | Operator | Description |
|------|----------|-------------|
| **NAM** (default) | `nms-operator` | Network is provisioned via the Network Automation Manager (NAM) API. NAM allocates VLAN IDs, IPv4/IPv6 prefixes, and manages the underlying network fabric. |
| **Manual** | `manual-ip-provisioning-operator` | Network parameters (CIDR, gateway, VLAN) are supplied directly in the `NetworkNamespace` spec and copied to status without contacting an external system. |

This operator watches `NetworkNamespace` resources and acts **only** on those configured for manual provisioning. It copies the user-supplied network configuration from `.spec.ipAllocation.static` into `.status` fields (`IPv4Prefix`, `VlanID`) and sets `provisioningPhase=Ready`, which unblocks downstream operators:

- **static-ip-operator** — allocates IPs from the manually-defined pool
- **kea-operator** — configures DHCP reservations for the subnet
- **kubevirt-operator** — creates network attachments and cloud-init configs

## Prerequisites

- Kubernetes 1.28+
- vitistack CRDs installed (specifically `NetworkNamespace`)
- Helm 3.x (for chart-based installation)

## Installation

### Helm

```bash
helm install manual-ip-provisioning-operator \
  ./charts/manual-ip-provisioning-operator \
  --namespace vitistack-system \
  --create-namespace
```

### From source

```bash
make build
./bin/manager
```

## Configuration

The operator is configured via environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `LOG_LEVEL` | Log verbosity (`debug`, `info`, `warn`, `error`) | `info` |
| `LOG_JSON` | Emit logs as JSON | `false` |
| `LOG_ADD_CALLER` | Include caller in log lines | `false` |
| `LOG_DISABLE_STACKTRACE` | Suppress stack traces | `false` |
| `LOG_UNESCAPED_MULTILINE` | Allow multiline log values | `false` |
| `LOG_COLORIZE_LINE` | Colorize log lines (dev only) | `false` |

## How It Works

### Detection

The operator identifies manual-provisioning `NetworkNamespace` resources by checking:

```
spec.ipAllocation.provider == "static-ip-operator"
```

In the v1alpha2 API, this maps to `spec.networkProvisioning.provider: manual`, which the conversion webhook translates to the v1alpha1 representation above.

### Reconciliation

1. **Skip** if the `NetworkNamespace` doesn't use manual provisioning (no-op for NAM-provisioned namespaces)
2. **Validate** that `spec.ipAllocation.static.ipv4CIDR` is set
3. **Copy** network parameters to status:
   - `spec.ipAllocation.static.ipv4CIDR` → `status.IPv4Prefix`
   - `spec.ipAllocation.static.vlanID` → `status.VlanID`
4. **Set** `status.Phase = Ready` and `status.ProvisioningPhase = Ready`
5. **Build** a `Ready` condition on the resource

### Example NetworkNamespace (v1alpha2)

```yaml
apiVersion: vitistack.io/v1alpha2
kind: NetworkNamespace
metadata:
  name: my-manual-network
  namespace: tenant-a
spec:
  datacenterIdentifier: dc-east-1
  supervisorIdentifier: sv-prod
  networkProvisioning:
    provider: manual
    manual:
      ipv4CIDR: "10.100.0.0/24"
      gateway: "10.100.0.1"
      vlanID: 200
  ipAllocation:
    type: static
```

### Example NetworkNamespace (v1alpha1)

```yaml
apiVersion: vitistack.io/v1alpha1
kind: NetworkNamespace
metadata:
  name: my-manual-network
  namespace: tenant-a
spec:
  datacenterIdentifier: dc-east-1
  supervisorIdentifier: sv-prod
  ipAllocation:
    type: static
    provider: static-ip-operator
    static:
      ipv4CIDR: "10.100.0.0/24"
      ipv4Gateway: "10.100.0.1"
      vlanID: 200
```

## Development

```bash
# Run tests
make test

# Build binary
make build

# Build container image
make docker-build IMG=ghcr.io/vitistack/viti-manual-ip-provisioning-operator:dev

# Lint
make lint
```

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.

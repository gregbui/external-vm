# Crossplane Composition: External VM with Static IP

This directory contains a **traditional Crossplane composition** (no custom functions)
that provisions a KubeVirt VirtualMachine on OpenShift Virtualization, attached to an
external network via Multus.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        User / Claim                             │
│                   ExternalVM.myorg.io                           │
└────────────────────────────┬────────────────────────────────────┘
                             │
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Composition (YAML)                         │
│                                                                 │
│  composedResources:                                             │
│    1. NetworkAttachmentDefinition (Multus)                      │
│       - CNI config with bridge, IPAM                            │
│       - Created by Kubernetes Provider                          │
│                                                                 │
│    2. VirtualMachine (KubeVirt)                                 │
│       - Domain: CPU, memory, disks                              │
│       - Networks: external (Multus) + pod                       │
│       - Volumes: containerDisk + cloudInitNoCloud               │
│       - Created by Kubernetes Provider                          │
│                                                                 │
│  patches:                                                       │
│    - FromCompositeFieldPath: XR → composed resources            │
│    - ToCompositeFieldPath: composed → XR status                 │
│    - Transforms: string formatting for CNI config               │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                  External Secrets (IPAM)                        │
│                                                                 │
│  ClusterSecretStore → IPAM API                                  │
│       │                                                         │
│       ▼                                                         │
│  ExternalSecret → vm-external-ip-secret                         │
│       │                                                         │
│       ▼                                                         │
│  Secret → available for composition patches                     │
└─────────────────────────────────────────────────────────────────┘
```

## Files

| File | Description |
|------|-------------|
| `xrd.yaml` | CompositeResourceDefinition — defines the `ExternalVM` XR |
| `composition.yaml` | Composition — composedResources + patches |
| `external-ip-alloc.yaml` | ExternalSecret + ClusterSecretStore for IP allocation |
| `claim.yaml` | Example claim — how users request an external VM |

## How It Works

### 1. Dynamic IP Allocation (External Secrets)

The External Secrets Operator reads IPs from an external IPAM:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: vm-external-ip
  namespace: default
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: ipam-store
    kind: ClusterSecretStore
  target:
    name: vm-external-ip-secret
  data:
    - secretKey: ipAddress
      remoteRef:
        key: /external-ips/next
        property: ip
    - secretKey: gateway
      remoteRef:
        key: /external-ips/config
        property: gateway
```

The IPAM API returns JSON: `{"ip": "10.20.30.100", "requestId": "abc123"}`

### 2. NetworkAttachmentDefinition

The composition creates a Multus NAD with:
- Bridge CNI plugin (`br-ex`)
- IPAM with static subnet/gateway
- Labels for tracking

### 3. VirtualMachine

The composition creates a KubeVirt VM with:
- ContainerDisk from specified image
- Cloud-init that configures static IP on `eth1`
- Network interface attached to the NAD via Multus
- Pod network for cluster communication

### 4. IP Configuration

The VM's cloud-init configures the static IP on boot:
```yaml
runcmd:
  - |
    nmcli con add type ethernet con-name external-iface ifname eth1 \
      ipv4.address 10.20.30.100/24 \
      ipv4.gateway 10.20.30.1 \
      ipv4.method manual
    nmcli con up external-iface
```

## Prerequisites

1. **OpenShift Virtualization** (KubeVirt) installed on the cluster
2. **Multus CNI** installed for external network attachment
3. **Crossplane** with Kubernetes Provider installed
4. **External Secrets Operator** installed (for dynamic IP allocation)
5. **IPAM** configured with an API endpoint (Infoblox, Kea, custom)

## Deployment

```bash
# 1. Install the XRD
kubectl apply -f xrd.yaml

# 2. Install the composition
kubectl apply -f composition.yaml

# 3. Install External IP Allocation
kubectl apply -f external-ip-alloc.yaml

# 4. Request an external VM
kubectl apply -f claim.yaml

# 5. Verify
kubectl get externalvm my-external-vm
kubectl get vm -n default
kubectl get networkattachmentdefinition -n default
kubectl get secret vm-external-ip -n default
```

## Result

After applying the claim, you'll have:

- An **ExternalVM** composite resource in `Ready` status
- A **VirtualMachine** in the `default` namespace, running with:
  - The specified CPU, memory, and disk
  - A network interface attached to the external network
  - Cloud-init configured to set the static IP on boot
- A **NetworkAttachmentDefinition** for Multus to attach the VM
- An **ExternalSecret** that keeps the IP in sync with the IPAM

## Patch Flow

```
XR spec fields ──────────────────────────────────────────────────┐
                                                                ▼
FromCompositeFieldPath patches ──► composedResources.base ──► Kubernetes API
                                                                │
                                                                ▼
ToCompositeFieldPath patches ◄─── composedResources.status ◄─── Kubernetes API
                                                                │
                                                                ▼
XR status fields
```

## Traditional vs. Composition Functions

| Aspect | Traditional (this) | Composition Functions |
|---|---|---|
| **Code** | Pure YAML | Go functions |
| **Deployment** | `kubectl apply` | Build + deploy containers |
| **Complexity** | Low | High |
| **Flexibility** | Limited to patches | Full Go programming |
| **Maintenance** | YAML only | Go + containers + tests |
| **Best for** | Simple resource creation | Complex orchestration |

This traditional approach is the **recommended starting point** for most use cases.
Only move to composition functions when you need logic that patches can't express.

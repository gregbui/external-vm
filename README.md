# Crossplane Composition: External VM with Static IP

Provisions a KubeVirt VirtualMachine on OpenShift Virtualization with a Multus-attached
external network and static IP allocation from Infoblox NIOS via WAPI.

## Quick Overview

| Aspect | Detail |
|---|---|
| **XR Kind** | `ExternalVM.myorg.io` (v1alpha1) |
| **Composition** | `external-vm-composition` (pipeline mode) |
| **Composed Resources** | NetworkAttachmentDefinition (Multus) + VirtualMachine (KubeVirt) |
| **IPAM** | Infoblox NIOS via WAPI (direct HTTP calls) |
| **Cloud-Init / SysPrep** | `ip-inject-fn` allocates IP + DNS, injects config based on `osType` (cloud-init for Linux, SysPrep ISO for Windows) |
| **Data Disks** | Handled by `data-disk-fn` pipeline step |

## Architecture

```
User Claim (ExternalVM)
        │
        ▼
┌─── Composition (Pipeline Mode) ──────────────────────┐
│                                                      │
│  composedResources:                                  │
│    1. NetworkAttachmentDefinition ──► Multus bridge  │
│    2. VirtualMachine        ──► KubeVirt VM          │
│                                                      │
│  patches:                                            │
│    FromCompositeFieldPath: XR spec -> VM/NAD fields  │
│    CombineFromCompositeFieldPath: rebuild NAD config │
│    FromComposedFieldPath:   NAD name -> VM network   │
│    ToCompositeFieldPath:    VM/NAD status -> XR      │
│                                                      │
│  pipeline:                                           │
│    1. data-disk-reconciler ──► data-disk-fn          │
│       (creates PVCs, patches VM disks/volumes)       │
│    2. ip-inject ──► ip-inject-fn                     │
│       (calls Infoblox WAPI, injects static IP)       │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌─── ip-inject-fn (osType branch) ─────────────────────┐
│  Linux:  cloud-init network-config v2 ──► eth1       │
│  Windows: SysPrep ISO (unattend.xml) ──► CD-ROM      │
└──────────────────────────────────────────────────────┘
        │
        ▼
┌─── Infoblox WAPI ────────────────────────────────────┐
│  ip-inject-fn ──► POST /wapi/v2.12/ipv4address       │
│       │                                              │
│       ▼                                              │
│  POST /wapi/v2.12/record:host (DNS)                  │
└──────────────────────────────────────────────────────┘
```

## Files

| File | Description |
|---|---|
| `xrd.yaml` | CompositeResourceDefinition — `ExternalVM.myorg.io`. Spec: vmName, cpu, memory, diskSize, osType, image, network config, externalIPPool, dataDisks[]. Status: externalIP, infobloxIPRef, nadName, vmUID, osType, dataDisks[]. |
| `composition.yaml` | Composition with pipeline mode. Patches: XR spec -> VM/NAD fields, NAD name -> VM network ref, VM/NAD status -> XR status. Pipeline steps: data-disk-fn + ip-inject-fn. |
| `infoblox-wapi.yaml` | Infoblox WAPI credentials (K8s Secret), CA cert (ConfigMap), and ClusterSecretStore for ExternalSecrets (legacy/optional). |
| `claim.yaml` | Example claim — 4 CPU, 8Gi RAM, 40Gi disk, 2 data disks (data: 50Gi, logs: 20Gi). |
| `functions/data-disk-fn/` | Pipeline function: creates PVCs for data disks, patches VM disks/volumes. |
| `functions/ip-inject-fn/` | Pipeline function: calls Infoblox WAPI for IP allocation + DNS, injects static network config (cloud-init for Linux, SysPrep ISO for Windows). |
| `.gitignore` | Git ignore rules. |

## Pipeline Functions

### data-disk-fn

Creates PVCs for the root disk (`spec.diskSize`) and each entry in `spec.dataDisks[]`.
Patches the VM with disk/volume entries. For Windows (`osType: windows`), adds a
`virtio-scsi` controller and uses `scsi` bus for the root disk. Data disks always use `virtio`.

**OS-specific disk configuration:**
| OS | Root Disk Bus | SCSI Controller |
|---|---|---|
| linux | `virtio` | none |
| windows | `scsi` | `virtio-scsi` (scsi0) |

**Directory structure:**
- `fn.go` — Function logic
- `fn_test.go` — Unit tests
- `render_test.go` — Composition render tests (run with `-claim-file=claim.yaml`)
- `main.go` — Entry point
- `Dockerfile` — Multi-stage BuildKit build
- `package/crossplane.yaml` — xpkg function package manifest
- `example/` — Example manifests for local `crossplane render` testing
- `.github/workflows/ci.yml` — CI pipeline (lint, test, build, push)
- `.golangci.yml` — Linter configuration
- `.xpkgignore` — Excludes non-package files from xpkg build

**Building:**
```bash
cd functions/data-disk-fn

# Run tests
go test ./... -v

# Build xpkg package
crossplane xpkg build -f package

# Run render test against a claim
go test -run TestRenderClaim -v -claim-file=../../claim.yaml
```

### ip-inject-fn

Allocates a static IP from Infoblox NIOS via WAPI and injects it into the VM based on `osType`:
- **Linux**: injects cloud-init `network-config v2` into `cloudInitNoCloud.userData`
- **Windows**: generates SysPrep `unattend.xml`, creates ISO, provisions as CD-ROM PVC

Allocates IP via Infoblox WAPI, creates DNS record, and stores IP reference for lifecycle management.

**Directory structure:**
- `fn.go` — Function logic
- `fn_test.go` — Unit tests
- `render_test.go` — Composition render tests (run with `-claim-file=claim.yaml`)
- `main.go` — Entry point
- `Dockerfile` — Multi-stage BuildKit build
- `package/crossplane.yaml` — xpkg function package manifest
- `example/` — Example manifests for local `crossplane render` testing
- `.github/workflows/ci.yml` — CI pipeline (lint, test, build, push)
- `.golangci.yml` — Linter configuration
- `.xpkgignore` — Excludes non-package files from xpkg build

**Building:**
```bash
cd functions/ip-inject-fn

# Run tests
go test ./... -v

# Build xpkg package
crossplane xpkg build -f package

# Run render test against a claim
go test -run TestRenderClaim -v -claim-file=../../claim.yaml
```

## How It Works

### 1. IP Allocation (Infoblox WAPI)

The `ip-inject-fn` pipeline function calls Infoblox WAPI directly:

1. **Allocate IP**: POST `/wapi/v2.12/ipv4address` with VM name, network view, and extattrs
2. **Create DNS**: POST `/wapi/v2.12/record:host` for reverse DNS lookup
3. **Store ref**: Infoblox `_ref` saved in XR `status.infobloxIPRef` for lifecycle management

Credentials are read from K8s Secret `infoblox-credentials` (namespace: `crossplane-system`).
The WAPI endpoint is configured via the `INFOBLOX_HOST` env var.

### 2. NetworkAttachmentDefinition (Multus)

Composition creates a NAD via `CombineFromCompositeFieldPath` — rebuilds the CNI JSON config from XR spec fields (`bridgeName`, `subnet`, `gateway`). Patches NAD `metadata.name` from `spec.networkName`.

### 3. VirtualMachine (KubeVirt)

Composition creates a KubeVirt VM with:
- `persistentVolumeClaim` root disk from `spec.diskSize` (created by data-disk-fn)
- `metadata.name` from `spec.vmName`, `metadata.namespace` from `spec.namespace`
- CPU cores from `spec.cpu`, memory from `spec.memory`
- `os.type` from `spec.osType` (mapped: `windows` → `windows.hyperv`)
- Multus network reference patched from NAD name (`FromComposedFieldPath`)
- Data disks appended by `data-disk-fn` (PVCs + disk/volume patches)
- SCSI controller + scsi root disk for Windows (handled by data-disk-fn)

### 4. Network Config Injection

After IP allocation, `ip-inject-fn` injects network configuration based on `osType`:

**Linux** — cloud-init `network-config v2` on `eth1`:
```yaml
network:
  config:
    version: 2
    ethernets:
      eth1:
        dhcp4: false
        addresses: ["<allocated-ip>/24"]
        nameservers:
          addresses: ["8.8.8.8", "1.1.1.1"]
        routes:
          - to: 0.0.0.0/0
            via: <gateway>
            metric: 100
```
Patches the VM's `cloudInitNoCloud.userData` and sets `status.externalIP`.

**Windows** — SysPrep ISO with `unattend.xml`:
- Generates `unattend.xml` with static IP, DNS, routes, hostname, and admin credentials
- Creates ISO9660 filesystem containing `unattend.xml`
- Provisions ISO as a PVC-backed CD-ROM disk (`sata` bus)
- Windows reads `unattend.xml` during first boot (specialize pass)

### 5. XR Status

- `status.nadName` <- NAD `metadata.name`
- `status.vmUID` <- VM `metadata.uid`
- `status.conditions` <- VM `status.conditions`
- `status.externalIP` <- set by `ip-inject-fn`
- `status.infobloxIPRef` <- Infoblox reference for IP release
- `status.osType` <- echoed from spec (linux or windows)
- `status.dataDisks` <- PVC names (root disk + data disks)

## Prerequisites

1. OpenShift Virtualization (KubeVirt)
2. Multus CNI
3. Crossplane + Kubernetes Provider
4. Infoblox NIOS with WAPI enabled
5. `crossplane-function-data-disk-fn` deployed (for data disk PVCs)
6. `crossplane-function-ip-inject-fn` deployed (for IP allocation + injection)

### Windows VM Requirements

- **Virtio drivers ISO** — Windows VMs require Virtio SCSI and network drivers. Provide a
  Virtio drivers ISO and attach it as a secondary CD-ROM disk.
- **Recommended diskSize** — Windows typically needs 60Gi+ for the OS disk.
- **Product key** — The SysPrep `unattend.xml` includes an empty `<ProductKey></ProductKey>`.
  Provide your own key or use KMS activation.
- **Admin password** — Default admin password is set in `unattend.xml`. Change for production.

## Deployment

```bash
kubectl apply -f xrd.yaml                          # 1. XRD
kubectl apply -f infoblox-wapi.yaml                # 2. Infoblox credentials + CA
kubectl apply -f functions/ip-inject-fn/           # 3. IP inject function
kubectl apply -f functions/data-disk-fn/           # 4. Data disk function
kubectl apply -f composition.yaml                  # 5. Composition
kubectl apply -f claim.yaml                        # 6. Claim
kubectl get externalvm my-external-vm              # 7. Verify
```

## Patch Reference

### XR spec -> VM
| Patch | XR Field | VM Field |
|---|---|---|
| vmName | `spec.vmName` | `metadata.name` |
| namespace | `spec.namespace` | `metadata.namespace` |
| cpu | `spec.cpu` | `spec.template.spec.domain.cpu.cores` |
| memory | `spec.memory` | `spec.template.spec.domain.resources.requests.memory` |
| osType | `spec.osType` | `spec.template.spec.domain.os.type` (windows → windows.hyperv) |

### XR spec -> NAD
| Patch | XR Field | NAD Field |
|---|---|---|
| networkName | `spec.networkName` | `metadata.name` |
| CNI config | `bridgeName`, `subnet`, `gateway` | `spec.config` (JSON rebuild) |

### Cross-resource
| Patch | From | To |
|---|---|---|
| NAD -> VM network | NAD `metadata.name` | VM `spec.template.spec.networks[0].multus.networkName` |

### Composed status -> XR status
| Patch | From | XR Field |
|---|---|---|
| NAD name | NAD `metadata.name` | `status.nadName` |
| VM uid | VM `metadata.uid` | `status.vmUID` |
| VM conditions | VM `status.conditions` | `status.conditions` |

## Infoblox Configuration

The function requires these env vars (set in the deployment):

| Env Var | Source | Description |
|---|---|---|
| `INFOBLOX_HOST` | Secret `infoblox-credentials.data.host` | WAPI endpoint hostname |
| `INFOBLOX_USER` | Secret `infoblox-credentials.data.username` | WAPI username |
| `INFOBLOX_PASSWORD` | Secret `infoblox-credentials.data.password` | WAPI password |
| `INFOBLOX_CA_CERT_PATH` | Env (mounted from ConfigMap) | Path to CA cert for self-signed certs |

The `infoblox-credentials` secret must also include a `host` key:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: infoblox-credentials
  namespace: crossplane-system
type: Opaque
stringData:
  host: "infoblox.example.com"
  username: "admin"
  password: "${INFOBLOX_PASSWORD}"
```

## Known Gaps / TODOs

- **IP release on VM deletion** — `status.infobloxIPRef` is stored but no finalizer releases the IP back to Infoblox when the VM is deleted. Add a finalizer + release logic.
- **networkView not required** — `spec.networkView` is optional; if omitted, Infoblox uses the default network view.
- **SysPrep ISO content provisioning** — The ISO PVC is created but the `unattend.xml` content
  must be written to the PVC via an init container or external process. The current implementation
  creates the PVC reference; production deployment needs ISO content provisioning.
- **Virtio drivers ISO** — Windows VMs require a Virtio drivers ISO attached as a CD-ROM.
  This is not yet automated; attach manually or add to composition.

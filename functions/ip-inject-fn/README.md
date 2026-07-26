# ip-inject-fn — Composition Function

Allocates a static IP from Infoblox NIOS via WAPI and injects it into the
VirtualMachine's cloud-init `userData` as a network-config v2 block.

## What It Does

1. Reads Infoblox credentials from the `infoblox-credentials` Secret
2. Calls Infoblox WAPI `POST /wapi/v2.12/ipv4address` to allocate an IP
3. Creates a DNS host record via `POST /wapi/v2.12/record:host`
4. Injects cloud-init network-config v2 into the VM's `cloudInitNoCloud.userData`:
   ```yaml
   network:
     config:
       version: 2
       ethernets:
         eth1:
           dhcp4: false
           addresses: ["<ip>/24"]
           routes:
             - to: 0.0.0.0/0
               via: <gateway>
   ```
5. Stores the allocated IP in XR `status.externalIP` and the Infoblox `_ref` in `status.infobloxIPRef`

On re-reconciliation, it reuses the previously allocated IP (stored in XR status)
instead of requesting a new one.

## Local Testing with `crossplane composition render`

### Prerequisites

```bash
# Install Crossplane CLI
brew install crossplane/tap/crossplane

# Ensure Go is installed
go version
```

### Quick Start

```bash
cd functions/ip-inject-fn

# Build the function
go build -o fn .

# Start the function (in a terminal)
INFOBLOX_HOST=infoblox.example.com INFOBLOX_USER=admin INFOBLOX_PASSWORD=secret \
  ./fn --insecure --address :9443

# In another terminal, render the composition
crossplane composition render \
    ../../claim.yaml \
    ../../composition.yaml \
    function.yaml
```

### What `render` Produces

The output shows the rendered composed resources:

- **NetworkAttachmentDefinition** — from the composition base
- **VirtualMachine** — from the composition base, **patched** with cloud-init network config
- No additional resources (IP allocation is external to K8s)

### Unit Tests

```bash
# Run unit tests (mock Infoblox, mock k8s client)
go test ./... -v

# Run render test against a claim file
go test -run TestRenderClaim -v -claim-file=../../claim.yaml
```

## Environment Variables

| Variable | Required | Description |
|---|---|---|
| `INFOBLOX_HOST` | Yes | Infoblox WAPI endpoint hostname |
| `INFOBLOX_USER` | No | WAPI username (default: `admin`) |
| `INFOBLOX_PASSWORD` | No | WAPI password |
| `INFOBLOX_CA_CERT_PATH` | No | Path to CA cert for self-signed certs |

## Infoblox Credentials Secret

The function reads credentials from a Kubernetes Secret:

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

## Function Runtime

The `function.yaml` includes the annotation:

```yaml
render.crossplane.io/runtime: "Development"
```

This tells the render engine to connect to the locally-running function on
`localhost:9443` instead of pulling a Docker image. The function must be
started with `--insecure` before rendering.

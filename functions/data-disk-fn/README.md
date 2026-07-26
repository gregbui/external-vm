# data-disk-fn — Composition Function

Creates PVCs for data disks and patches the VirtualMachine resource with disk/volume entries.

## Local Testing with `crossplane composition render`

The Crossplane CLI provides `composition render` for offline testing of compositions
and their functions.

### Prerequisites

```bash
# Install Crossplane CLI
brew install crossplane/tap/crossplane

# Install Docker Desktop (render uses Docker by default)
brew install --cask docker

# Ensure Go is installed
go version
```

### Quick Start

```bash
cd functions/data-disk-fn

# Build and render with the default claim
./render.sh

# Render with a multi-disk claim variant
./render.sh claim-multi-disk.yaml
```

### Manual Steps

```bash
cd functions/data-disk-fn

# 1. Build the function
go build -o fn .

# 2. Start the function (in a terminal)
./fn --insecure --address :9443

# 3. In another terminal, render the composition
#    Order: <claim> <composition> [<function>]
crossplane composition render \
    ../../claim.yaml \
    ../../composition.yaml \
    function.yaml
```

### What `render` Produces

The output shows the rendered composed resources that would be created:

- **NetworkAttachmentDefinition** — from the composition base
- **VirtualMachine** — from the composition base, **patched** with data disk entries
- **PersistentVolumeClaim(s)** — created by the data-disk-fn for each data disk

### Test Claims

| File | Description |
|------|-------------|
| `../../claim.yaml` | Default claim — 2 data disks (data + logs) |
| `claim-multi-disk.yaml` | Multi-disk variant — 2 data disks with different storage classes |
| `claim-no-disks.yaml` | No data disks — tests the early-return path |

### Function Runtime

The `function.yaml` includes the annotation:

```yaml
render.crossplane.io/runtime: "Development"
```

This tells the render engine to connect to the locally-running function on
`localhost:9443` instead of pulling a Docker image. The function must be
started with `--insecure` before rendering.

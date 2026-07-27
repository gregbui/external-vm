# Example manifests

You can run your function locally and test it using `crossplane render`
with these example manifests.

```shell
# Run the function locally
$ go run . --insecure --debug
```

```shell
# Then, in another terminal, call it with these example manifests
$ crossplane render xr.yaml composition.yaml functions.yaml -r
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: example-vm
spec:
  template:
    spec:
      volumes:
        - cloudInitNoCloud:
            userData: |
              #cloud-config
              hostname: example-vm
              network:
                config:
                  version: 2
                  ethernets:
                    eth1:
                      dhcp4: false
                      addresses: ["10.20.30.100/24"]
```

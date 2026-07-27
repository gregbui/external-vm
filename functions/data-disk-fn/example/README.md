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
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-external-vm-data
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 50Gi
```

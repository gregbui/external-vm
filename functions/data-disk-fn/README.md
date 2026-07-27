# data-disk-fn
[![CI](https://github.com/crossplane/function-template-go/actions/workflows/ci.yml/badge.svg)](https://github.com/crossplane/function-template-go/actions/workflows/ci.yml)

A [composition function][functions] that creates PVCs for data disks and patches
the VirtualMachine resource with disk/volume entries.

To learn how to use this function:

* [Follow the guide to writing a composition function in Go][function guide]
* [Learn about how composition functions work][functions]
* [Read the function-sdk-go package documentation][package docs]

If you just want to jump in and get started:

1. Update `fn.go` with your logic
1. Add tests for your logic in `fn_test.go`
1. Update this file, `README.md`, to be about your function!

This function uses [Go][go], [Docker][docker], and the [Crossplane CLI][cli] to
build functions.

```shell
# Run tests - see fn_test.go
$ go test ./...

# Build the function's runtime image - see Dockerfile
$ docker build . --tag=runtime

# Build a function package - see package/crossplane.yaml
$ crossplane xpkg build -f package --embed-runtime-image=runtime
```

[functions]: https://docs.crossplane.io/latest/concepts/composition-functions
[go]: https://go.dev
[function guide]: https://docs.crossplane.io/knowledge-base/guides/write-a-composition-function-in-go
[package docs]: https://pkg.go.dev/github.com/crossplane/function-sdk-go
[docker]: https://www.docker.com
[cli]: https://docs.crossplane.io/latest/cli

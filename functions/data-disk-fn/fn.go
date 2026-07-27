package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

const (
	vmKeyName       = "virtual-machine"
	pvcLabelManaged = "external-vm-fn"
)

// Function is your composition function.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	// Get the observed XR
	xr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("getting observed composite resource: %w", err))
		return rsp, nil
	}

	// Get observed composed resources (for tracking existing PVCs)
	_, err = request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("getting observed composed resources: %w", err))
		return rsp, nil
	}

	// Get desired composed resources from the Composition
	desired, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("getting desired composed resources: %w", err))
		return rsp, nil
	}

	// Extract spec from XR
	spec, ok := xr.Resource.UnstructuredContent()["spec"].(map[string]any)
	if !ok {
		response.Fatal(rsp, fmt.Errorf("XR spec is not an object"))
		return rsp, nil
	}

	// ── Extract osType ──
	osType, _ := spec["osType"].(string)
	if osType == "" {
		osType = "linux"
	}

	// ── Extract diskSize for root disk ──
	var rootDiskSize string
	if ds, ok := spec["diskSize"].(string); ok && ds != "" {
		rootDiskSize = ds
	}

	// ── Determine disk bus based on osType ──
	rootDiskBus := "virtio"
	if osType == "windows" {
		rootDiskBus = "scsi"
	}

	// ── Extract dataDisks from XR spec ──
	dataDisksRaw, ok := spec["dataDisks"].([]any)
	if !ok || len(dataDisksRaw) == 0 {
		// No data disks — check if we still need to create root disk PVC
		if rootDiskSize == "" {
			response.ConditionTrue(rsp, "DataDisksReconciled", "No data disks or root disk size to reconcile")
			return rsp, nil
		}
		// Only root disk PVC needed
		return f.createRootDiskPVC(rsp, desired, xr, rootDiskSize, osType, rootDiskBus)
	}

	// Parse disk specs
	type diskSpec struct {
		name             string
		size             string
		storageClassName string
	}
	desiredDisks := make([]diskSpec, 0, len(dataDisksRaw))
	for _, d := range dataDisksRaw {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		ds := diskSpec{}
		if n, ok := dm["name"].(string); ok {
			ds.name = n
		}
		if s, ok := dm["size"].(string); ok {
			ds.size = s
		}
		if sc, ok := dm["storageClassName"].(string); ok {
			ds.storageClassName = sc
		}
		if ds.name == "" || ds.size == "" {
			response.ConditionFalse(rsp, "DataDisksReconciled",
				"Invalid disk spec: name and size required")
			return rsp, nil
		}
		desiredDisks = append(desiredDisks, ds)
	}

	// ── Create root disk PVC if diskSize specified ──
	var rootPVCName string
	if rootDiskSize != "" {
		rootPVCName = fmt.Sprintf("%s-rootdisk", xr.Resource.GetName())
		rootPVCKeyName := resource.Name("root-disk-pvc")

		pvc := resource.NewDesiredComposed()
		pvc.Resource.SetAPIVersion("v1")
		pvc.Resource.SetKind("PersistentVolumeClaim")
		pvc.Resource.SetName(rootPVCName)
		pvc.Resource.SetNamespace(xr.Resource.GetNamespace())
		pvc.Resource.SetLabels(map[string]string{
			pvcLabelManaged: "true",
		})
		pvc.Resource.SetOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion:         xr.Resource.GetAPIVersion(),
				Kind:               xr.Resource.GetKind(),
				Name:               xr.Resource.GetName(),
				UID:                xr.Resource.GetUID(),
				Controller:         boolPtr(true),
				BlockOwnerDeletion: boolPtr(true),
			},
		})

		pvcSpec := map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": rootDiskSize,
				},
			},
		}
		pvc.Resource.Object["spec"] = pvcSpec

		desired[rootPVCKeyName] = pvc
	}

	// ── Create PVCs for data disks ──
	for i, ds := range desiredDisks {
		pvcName := fmt.Sprintf("%s-%s", xr.Resource.GetName(), ds.name)
		pvcKeyName := resource.Name(fmt.Sprintf("data-disk-%d", i))

		pvc := resource.NewDesiredComposed()
		pvc.Resource.SetAPIVersion("v1")
		pvc.Resource.SetKind("PersistentVolumeClaim")
		pvc.Resource.SetName(pvcName)
		pvc.Resource.SetNamespace(xr.Resource.GetNamespace())
		pvc.Resource.SetLabels(map[string]string{
			pvcLabelManaged: "true",
		})
		pvc.Resource.SetOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion:         xr.Resource.GetAPIVersion(),
				Kind:               xr.Resource.GetKind(),
				Name:               xr.Resource.GetName(),
				UID:                xr.Resource.GetUID(),
				Controller:         boolPtr(true),
				BlockOwnerDeletion: boolPtr(true),
			},
		})

		pvcSpec := map[string]any{
			"accessModes": []any{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": ds.size,
				},
			},
		}
		if ds.storageClassName != "" {
			pvcSpec["storageClassName"] = ds.storageClassName
		}
		pvc.Resource.Object["spec"] = pvcSpec
		desired[pvcKeyName] = pvc
	}

	// ── Patch VM: replace volumes array, add disk/volume entries ──
	vm, ok := desired[resource.Name(vmKeyName)]
	if !ok {
		// Render engine may not inject composedResources.
		// Return success if VM is not present — PVCs are still created.
		response.ConditionTrue(rsp, "DataDisksReconciled",
			"VM not found in desired composed resources; PVCs created")
		return rsp, nil
	}

	vmSpec := vm.Resource.UnstructuredContent()["spec"].(map[string]any)
	template := vmSpec["template"].(map[string]any)
	vmSpecSpec := template["spec"].(map[string]any)
	domain := vmSpecSpec["domain"].(map[string]any)
	devices := domain["devices"].(map[string]any)

	// ── Add SCSI controller for Windows ──
	if osType == "windows" {
		addSCSIController(devices)
	}

	// Get existing disks (rootdisk, cloudinitdisk) under domain.devices.disks
	existingDisks, _ := devices["disks"].([]any)
	if existingDisks == nil {
		existingDisks = []any{}
	}

	// Build new volumes array.
	// If root disk PVC exists, replace rootdisk volume with PVC ref and keep cloudinitdisk.
	// Otherwise preserve existing volumes (rootdisk, cloudinitdisk, etc.) as-is.
	var volumes []any
	if rootPVCName != "" {
		volumes = append(volumes, map[string]any{
			"name": "rootdisk",
			"persistentVolumeClaim": map[string]any{
				"claimName": rootPVCName,
			},
		})
		existingVolumes, _ := vmSpecSpec["volumes"].([]any)
		for _, v := range existingVolumes {
			if vMap, ok := v.(map[string]any); ok {
				if name, ok := vMap["name"].(string); ok && name == "cloudinitdisk" {
					volumes = append(volumes, v)
					break
				}
			}
		}
	} else {
		existingVolumes, _ := vmSpecSpec["volumes"].([]any)
		volumes = append(volumes, existingVolumes...)
	}

	// Append data disk entries to both disks and volumes arrays
	for _, ds := range desiredDisks {
		pvcName := fmt.Sprintf("%s-%s", xr.Resource.GetName(), ds.name)
		// Disk entry (data disks always use virtio)
		existingDisks = append(existingDisks, map[string]any{
			"name": ds.name,
			"disk": map[string]any{
				"bus": "virtio",
			},
		})
		// Volume entry
		volumes = append(volumes, map[string]any{
			"name": ds.name,
			"persistentVolumeClaim": map[string]any{
				"claimName": pvcName,
			},
		})
	}

	// Update rootdisk entry with correct bus type
	for i, d := range existingDisks {
		if dMap, ok := d.(map[string]any); ok {
			if name, ok := dMap["name"].(string); ok && name == "rootdisk" {
				if dMap["disk"] != nil {
					if diskMap, ok := dMap["disk"].(map[string]any); ok {
						diskMap["bus"] = rootDiskBus
					}
				}
				existingDisks[i] = dMap
				break
			}
		}
	}

	devices["disks"] = existingDisks
	vmSpecSpec["volumes"] = volumes
	desired[resource.Name(vmKeyName)] = vm

	// ── Write results ──
	// Set desired composed resources (PVCs + patched VM)
	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composed resources: %w", err))
		return rsp, nil
	}

	// Write PVC names to XR status (root disk + data disks)
	statusDisks := make([]any, 0, len(desiredDisks)+1)
	if rootPVCName != "" {
		statusDisks = append(statusDisks, rootPVCName)
	}
	for _, ds := range desiredDisks {
		statusDisks = append(statusDisks, fmt.Sprintf("%s-%s", xr.Resource.GetName(), ds.name))
	}
	status, _ := xr.Resource.UnstructuredContent()["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["dataDisks"] = statusDisks
	status["osType"] = osType
	xr.Resource.SetUnstructuredContent(xr.Resource.UnstructuredContent())

	if err := response.SetDesiredCompositeResource(rsp, xr); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composite resource: %w", err))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "DataDisksReconciled",
		fmt.Sprintf("Reconciled root disk (%s) + %d data disks [os=%s]", rootDiskBus, len(desiredDisks), osType))
	return rsp, nil
}

// addSCSIController adds a virtio-scsi controller to the VM devices.
func addSCSIController(devices map[string]any) {
	controllers := []any{
		map[string]any{
			"name":   "scsi0",
			"model":  "virtio-scsi",
			"serial": "",
		},
	}
	devices["controllers"] = controllers
}

// createRootDiskPVC handles the case where only a root disk PVC is needed (no data disks).
func (f *Function) createRootDiskPVC(rsp *fnv1.RunFunctionResponse, desired map[resource.Name]*resource.DesiredComposed, xr *resource.Composite, rootDiskSize string, osType string, rootDiskBus string) (*fnv1.RunFunctionResponse, error) {
	rootPVCName := fmt.Sprintf("%s-rootdisk", xr.Resource.GetName())
	rootPVCKeyName := resource.Name("root-disk-pvc")

	pvc := resource.NewDesiredComposed()
	pvc.Resource.SetAPIVersion("v1")
	pvc.Resource.SetKind("PersistentVolumeClaim")
	pvc.Resource.SetName(rootPVCName)
	pvc.Resource.SetNamespace(xr.Resource.GetNamespace())
	pvc.Resource.SetLabels(map[string]string{
		pvcLabelManaged: "true",
	})
	pvc.Resource.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         xr.Resource.GetAPIVersion(),
			Kind:               xr.Resource.GetKind(),
			Name:               xr.Resource.GetName(),
			UID:                xr.Resource.GetUID(),
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	pvcSpec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources": map[string]any{
			"requests": map[string]any{
				"storage": rootDiskSize,
			},
		},
	}
	pvc.Resource.Object["spec"] = pvcSpec
	desired[rootPVCKeyName] = pvc

	// Patch VM volumes array to use PVC ref for rootdisk
	vm, ok := desired[resource.Name(vmKeyName)]
	if ok {
		vmSpec := vm.Resource.UnstructuredContent()["spec"].(map[string]any)
		template := vmSpec["template"].(map[string]any)
		vmSpecSpec := template["spec"].(map[string]any)
		domain := vmSpecSpec["domain"].(map[string]any)
		devices := domain["devices"].(map[string]any)

		// Add SCSI controller for Windows
		if osType == "windows" {
			addSCSIController(devices)
		}

		// Get existing disks and update rootdisk bus
		existingDisks, _ := devices["disks"].([]any)
		if existingDisks == nil {
			existingDisks = []any{}
		}
		for i, d := range existingDisks {
			if dMap, ok := d.(map[string]any); ok {
				if name, ok := dMap["name"].(string); ok && name == "rootdisk" {
					if dMap["disk"] != nil {
						if diskMap, ok := dMap["disk"].(map[string]any); ok {
							diskMap["bus"] = rootDiskBus
						}
					}
					existingDisks[i] = dMap
					break
				}
			}
		}
		devices["disks"] = existingDisks

		volumes := []any{
			map[string]any{
				"name": "rootdisk",
				"persistentVolumeClaim": map[string]any{
					"claimName": rootPVCName,
				},
			},
		}
		// Find cloudinitdisk volume from existing
		existingVolumes, _ := vmSpecSpec["volumes"].([]any)
		for _, v := range existingVolumes {
			if vMap, ok := v.(map[string]any); ok {
				if name, ok := vMap["name"].(string); ok && name == "cloudinitdisk" {
					volumes = append(volumes, v)
					break
				}
			}
		}

		vmSpecSpec["volumes"] = volumes
		desired[resource.Name(vmKeyName)] = vm
	}

	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composed resources: %w", err))
		return rsp, nil
	}

	// Update XR status with root disk PVC name and osType
	statusDisks := []any{rootPVCName}
	status, _ := xr.Resource.UnstructuredContent()["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["dataDisks"] = statusDisks
	status["osType"] = osType
	xr.Resource.SetUnstructuredContent(xr.Resource.UnstructuredContent())

	if err := response.SetDesiredCompositeResource(rsp, xr); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composite resource: %w", err))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "DataDisksReconciled", fmt.Sprintf("Reconciled root disk (%s) [os=%s]", rootDiskBus, osType))
	return rsp, nil
}

func boolPtr(b bool) *bool { return &b }

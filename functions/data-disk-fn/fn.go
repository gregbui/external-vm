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

	// Extract dataDisks from XR spec
	spec, ok := xr.Resource.UnstructuredContent()["spec"].(map[string]any)
	if !ok {
		response.Fatal(rsp, fmt.Errorf("XR spec is not an object"))
		return rsp, nil
	}

	dataDisksRaw, ok := spec["dataDisks"].([]any)
	if !ok || len(dataDisksRaw) == 0 {
		// No data disks — nothing to do
		response.ConditionTrue(rsp, "DataDisksReconciled", "No data disks to reconcile")
		return rsp, nil
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

	// ── Create PVCs for each data disk ──
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

	// ── Patch VM: add data disk entries to disks and volumes arrays ──
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

	// Get existing disks (rootdisk, cloudinitdisk) under domain.devices.disks
	devices := domain["devices"].(map[string]any)
	existingDisks, _ := devices["disks"].([]any)
	if existingDisks == nil {
		existingDisks = []any{}
	}

	// Get existing volumes (rootdisk, cloudinitdisk) at spec.template.spec level
	existingVolumes, _ := vmSpecSpec["volumes"].([]any)
	if existingVolumes == nil {
		existingVolumes = []any{}
	}

	// Append data disk entries
	for _, ds := range desiredDisks {
		pvcName := fmt.Sprintf("%s-%s", xr.Resource.GetName(), ds.name)
		// Disk entry
		existingDisks = append(existingDisks, map[string]any{
			"name": ds.name,
			"disk": map[string]any{
				"bus": "virtio",
			},
		})
		// Volume entry
		existingVolumes = append(existingVolumes, map[string]any{
			"name": ds.name,
			"persistentVolumeClaim": map[string]any{
				"claimName": pvcName,
			},
		})
	}

	devices["disks"] = existingDisks
	vmSpecSpec["volumes"] = existingVolumes
	desired[resource.Name(vmKeyName)] = vm

	// ── Write results ──
	// Set desired composed resources (PVCs + patched VM)
	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composed resources: %w", err))
		return rsp, nil
	}

	// Write PVC names to XR status
	statusDisks := make([]any, len(desiredDisks))
	for i, ds := range desiredDisks {
		statusDisks[i] = fmt.Sprintf("%s-%s", xr.Resource.GetName(), ds.name)
	}
	status, _ := xr.Resource.UnstructuredContent()["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["dataDisks"] = statusDisks
	xr.Resource.SetUnstructuredContent(xr.Resource.UnstructuredContent())

	if err := response.SetDesiredCompositeResource(rsp, xr); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composite resource: %w", err))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "DataDisksReconciled",
		fmt.Sprintf("Reconciled %d data disks", len(desiredDisks)))
	return rsp, nil
}

func boolPtr(b bool) *bool { return &b }

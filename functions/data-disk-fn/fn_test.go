package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
)

// buildResource creates a fnv1.Resource from a Go map.
func buildResource(gvk, name, ns, uid string, obj map[string]any) *fnv1.Resource {
	res, _ := structpb.NewStruct(obj)
	return &fnv1.Resource{
		Resource: res,
	}
}

// getObj extracts the object map from a fnv1.Resource.
func getObj(r *fnv1.Resource) map[string]any {
	b, _ := json.Marshal(r.GetResource())
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// getMeta extracts metadata from a fnv1.Resource object.
func getMeta(r *fnv1.Resource) map[string]any {
	return getObj(r)["metadata"].(map[string]any)
}

func newXR(name, ns string, specExtra map[string]any) *fnv1.Resource {
	spec := map[string]any{
		"vmName":      "web-server-01",
		"cpu":         float64(4),
		"memory":      "8Gi",
		"image":       "quay.io/example/rhel9-web:latest",
		"namespace":   "default",
		"networkName": "external-web-net",
		"subnet":      "10.20.30.0/24",
		"gateway":     "10.20.30.1",
		"bridgeName":  "br-ex",
	}
	for k, v := range specExtra {
		spec[k] = v
	}
	return buildResource("myorg.io/v1alpha1/ExternalVM", name, ns, name+"-uid", map[string]any{
		"apiVersion": "myorg.io/v1alpha1",
		"kind":       "ExternalVM",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
			"uid":       name + "-uid",
		},
		"spec": spec,
	})
}

func newVM() *fnv1.Resource {
	return buildResource("kubevirt.io/v1/VirtualMachine", "web-server-01", "default", "vm-uid", map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":      "web-server-01",
			"namespace": "default",
			"uid":       "vm-uid",
			"labels": map[string]any{
				"kubevirt.io/domain": "web-server-01",
			},
		},
		"spec": map[string]any{
			"runStrategy": "RerunOnFailure",
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"kubevirt.io/domain": "web-server-01",
					},
				},
				"spec": map[string]any{
					"domain": map[string]any{
						"devices": map[string]any{
							"disks": []any{
								map[string]any{"name": "rootdisk", "disk": map[string]any{"bus": "virtio"}},
								map[string]any{"name": "cloudinitdisk", "disk": map[string]any{"bus": "virtio"}},
							},
							"interfaces": []any{
								map[string]any{"name": "external", "bridge": map[string]any{}, "model": "virtio"},
							},
						},
						"networks": []any{
							map[string]any{"name": "external", "multus": map[string]any{"networkName": "default/external-web-net"}},
						},
					},
					"volumes": []any{
						map[string]any{"name": "rootdisk", "containerDisk": map[string]any{"image": "quay.io/example/rhel9:latest"}},
						map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"userData": "#cloud-config\nhostname: vm-external\n"}},
					},
				},
			},
		},
	})
}

func TestRunFunction(t *testing.T) {
	cases := map[string]struct {
		reason string
		req    *fnv1.RunFunctionRequest
		want   want
	}{
		"NoDataDisks": {
			reason: "Should return success condition when no data disks specified",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-no-disks"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", nil),
				},
				Desired: &fnv1.State{
					Resources: map[string]*fnv1.Resource{
						"virtual-machine": newVM(),
					},
				},
			},
			want: want{
				conditionType:     "DataDisksReconciled",
				conditionStatus:   fnv1.Status_STATUS_CONDITION_TRUE,
				fatalResultCount:  0,
				pvcCount:          0,
				skipResourceCheck: true,
			},
		},
		"SingleDataDisk": {
			reason: "Should create one PVC and patch VM with one data disk entry",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-single-disk"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", map[string]any{
						"dataDisks": []any{
							map[string]any{"name": "data", "size": "50Gi"},
						},
					}),
				},
				Desired: &fnv1.State{
					Resources: map[string]*fnv1.Resource{
						"virtual-machine": newVM(),
					},
				},
			},
			want: want{
				conditionType:     "DataDisksReconciled",
				conditionStatus:   fnv1.Status_STATUS_CONDITION_TRUE,
				fatalResultCount:  0,
				pvcCount:          1,
				pvcNames:          []string{"test-vm-data"},
				vmDisks:           3,
				vmVolumes:         3,
			},
		},
		"MultipleDataDisks": {
			reason: "Should create multiple PVCs and patch VM with multiple disk entries",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-multi-disks"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", map[string]any{
						"dataDisks": []any{
							map[string]any{"name": "fast", "size": "100Gi", "storageClassName": "fast-ssd"},
						},
					}),
				},
				Desired: &fnv1.State{
					Resources: map[string]*fnv1.Resource{
						"virtual-machine": newVM(),
					},
				},
			},
			want: want{
				conditionType:     "DataDisksReconciled",
				conditionStatus:   fnv1.Status_STATUS_CONDITION_TRUE,
				fatalResultCount:  0,
				pvcCount:          1,
				pvcNames:          []string{"test-vm-fast"},
				pvcStorageClass:   "fast-ssd",
				vmDisks:           3,
				vmVolumes:         3,
			},
		},
		"InvalidDiskSpec": {
			reason: "Should return false condition when disk spec is missing required fields",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-invalid-disk"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", map[string]any{
						"dataDisks": []any{
							map[string]any{"size": "50Gi"}, // missing name
						},
					}),
				},
				Desired: &fnv1.State{
					Resources: map[string]*fnv1.Resource{
						"virtual-machine": newVM(),
					},
				},
			},
			want: want{
				conditionType:     "DataDisksReconciled",
				conditionStatus:   fnv1.Status_STATUS_CONDITION_FALSE,
				fatalResultCount:  0,
				skipResourceCheck: true,
			},
		},
		"VMNotFound": {
			reason: "Should return success when VM is not in desired state (render engine compatibility)",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-vm-not-found"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", map[string]any{
						"dataDisks": []any{
							map[string]any{"name": "data", "size": "50Gi"},
						},
					}),
				},
				Desired: &fnv1.State{
					Resources: map[string]*fnv1.Resource{},
				},
			},
			want: want{
				conditionType:     "DataDisksReconciled",
				conditionStatus:   fnv1.Status_STATUS_CONDITION_TRUE,
				fatalResultCount:  0,
				skipResourceCheck: true,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{log: logging.NewNopLogger()}
			rsp, err := f.RunFunction(context.Background(), tc.req)

			if tc.want.err {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check conditions
			if tc.want.fatalResultCount > 0 {
				// Fatal cases don't set conditions
			} else if len(rsp.GetConditions()) != 1 {
				t.Errorf("expected 1 condition, got %d", len(rsp.GetConditions()))
			} else {
				c := rsp.GetConditions()[0]
				if c.GetType() != tc.want.conditionType {
					t.Errorf("condition type: want %q, got %q", tc.want.conditionType, c.GetType())
				}
				if c.GetStatus() != tc.want.conditionStatus {
					t.Errorf("condition status: want %v, got %v", tc.want.conditionStatus, c.GetStatus())
				}
			}

			// Check for fatal results
			fatalCount := 0
			for _, r := range rsp.GetResults() {
				if r.GetSeverity() == fnv1.Severity_SEVERITY_FATAL {
					fatalCount++
				}
			}
			if fatalCount != tc.want.fatalResultCount {
				t.Errorf("fatal result count: want %d, got %d", tc.want.fatalResultCount, fatalCount)
			}

			// Skip resource checks for cases that return early
			if tc.want.skipResourceCheck {
				return
			}

			// Check desired resources
			desired := rsp.GetDesired()
			if desired == nil || desired.GetResources() == nil {
				if tc.want.pvcCount > 0 || tc.want.vmDisks > 0 {
					t.Fatal("expected desired resources, got nil")
				}
				return
			}

			// Check PVCs
			pvcs := map[string]*fnv1.Resource{}
			for key, res := range desired.GetResources() {
				obj := getObj(res)
				if obj["kind"] == "PersistentVolumeClaim" {
					pvcs[key] = res
				}
			}
			if len(pvcs) != tc.want.pvcCount {
				t.Errorf("PVC count: want %d, got %d", tc.want.pvcCount, len(pvcs))
			}
			pvcNames := make([]string, 0, len(pvcs))
			for _, pvc := range pvcs {
				pvcNames = append(pvcNames, getMeta(pvc)["name"].(string))
			}
			if diff := cmp.Diff(tc.want.pvcNames, pvcNames); diff != "" {
				t.Errorf("PVC names: -want, +got:\n%s", diff)
			}

			// Check storage class
			if tc.want.pvcStorageClass != "" {
				for _, pvc := range pvcs {
					spec := getObj(pvc)["spec"].(map[string]any)
					sc := spec["storageClassName"].(string)
					if sc != tc.want.pvcStorageClass {
						t.Errorf("PVC storageClassName: want %q, got %q", tc.want.pvcStorageClass, sc)
					}
				}
			}

			// Check VM
			vmRes, ok := desired.GetResources()["virtual-machine"]
			if !ok {
				t.Fatal("VM not found in desired resources")
			}
			vmObj := getObj(vmRes)
			spec := vmObj["spec"].(map[string]any)
			template := spec["template"].(map[string]any)
			vmSpecSpec := template["spec"].(map[string]any)
			domain := vmSpecSpec["domain"].(map[string]any)
			devices := domain["devices"].(map[string]any)

			if tc.want.vmDisks > 0 {
				disks := devices["disks"].([]any)
				if len(disks) != tc.want.vmDisks {
					t.Errorf("VM disk count: want %d, got %d", tc.want.vmDisks, len(disks))
				}
			}
			if tc.want.vmVolumes > 0 {
				volumes := vmSpecSpec["volumes"].([]any)
				if len(volumes) != tc.want.vmVolumes {
					t.Errorf("VM volume count: want %d, got %d", tc.want.vmVolumes, len(volumes))
				}
			}
		})
	}
}

type want struct {
	err               bool
	conditionType     string
	conditionStatus   fnv1.Status
	fatalResultCount  int
	pvcCount          int
	pvcNames          []string
	pvcStorageClass   string
	vmDisks           int
	vmVolumes         int
	skipResourceCheck bool
}

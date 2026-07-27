package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"
)

var claimFile string

func init() {
	flag.StringVar(&claimFile, "claim-file", "", "Path to claim YAML file")
}

func TestRenderClaim(t *testing.T) {
	if claimFile == "" {
		fmt.Println("Usage: go test ./functions/data-disk-fn -run TestRenderClaim -v -claim-file=claim.yaml")
		t.Skip("no -claim-file set")
	}

	claimBytes, err := os.ReadFile(claimFile)
	if err != nil {
		t.Fatalf("Error reading claim: %v", err)
	}

	var claimMap map[string]any
	if err := yaml.Unmarshal(claimBytes, &claimMap); err != nil {
		t.Fatalf("Error parsing claim YAML: %v", err)
	}

	xrRes, _ := structpb.NewStruct(claimMap)

	vmName := getString(claimMap, "spec.vmName", "vm-external")
	namespace := getString(claimMap, "spec.namespace", "default")
	networkName := getString(claimMap, "spec.networkName", "external-web-net")

	vmMap := map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name":      vmName,
			"namespace": namespace,
			"uid":       "vm-uid",
			"labels": map[string]any{
				"kubevirt.io/domain": vmName,
			},
		},
		"spec": map[string]any{
			"runStrategy": "RerunOnFailure",
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"kubevirt.io/domain": vmName,
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
							map[string]any{"name": "external", "multus": map[string]any{"networkName": "default/" + networkName}},
						},
					},
					"volumes": []any{
						map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": ""}},
						map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"userData": "#cloud-config\nhostname: " + vmName + "\n"}},
					},
				},
			},
		},
	}

	vmRes, _ := structpb.NewStruct(vmMap)

	req := &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "local-render"},
		Observed: &fnv1.State{
			Composite: &fnv1.Resource{
				Resource: xrRes,
			},
		},
		Desired: &fnv1.State{
			Resources: map[string]*fnv1.Resource{
				"virtual-machine": {
					Resource: vmRes,
				},
			},
		},
	}

	f := &Function{log: logging.NewNopLogger()}
	rsp, err := f.RunFunction(context.Background(), req)
	if err != nil {
		t.Fatalf("Error running function: %v", err)
	}

	conds := rsp.GetConditions()
	fmt.Println("=== CONDITIONS ===")
	for _, c := range conds {
		fmt.Printf("  %s: %s (%s)\n", c.GetType(), c.GetReason(), c.GetMessage())
	}

	desired := rsp.GetDesired()
	if desired == nil || desired.GetResources() == nil {
		fmt.Println("\nNo desired resources.")
		return
	}

	fmt.Println("\n=== DESIRED RESOURCES ===")
	for key, res := range desired.GetResources() {
		obj := res.GetResource()
		if obj == nil {
			continue
		}
		b, _ := json.Marshal(obj.AsMap())
		var raw any
		json.Unmarshal(b, &raw)
		yamlBytes, _ := yaml.Marshal(raw)

		kind := getStr(raw, "kind")
		name := getStr(raw, "metadata.name")
		fmt.Printf("--- %s (%s/%s) ---\n", key, kind, name)
		fmt.Println(string(yamlBytes))
	}
}

func getString(m map[string]any, path string, def string) string {
	keys := strings.Split(path, ".")
	cur := any(m)
	for _, k := range keys {
		switch v := cur.(type) {
		case map[string]any:
			cur = v[k]
		default:
			return def
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return def
}

func getStr(v any, path string) string {
	keys := strings.Split(path, ".")
	cur := v
	for _, k := range keys {
		switch m := cur.(type) {
		case map[string]any:
			cur = m[k]
		default:
			return ""
		}
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return ""
}

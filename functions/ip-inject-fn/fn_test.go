package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

type stubSecret struct {
	metav1.TypeMeta
	metav1.ObjectMeta
	Data map[string][]byte
}

func (s *stubSecret) DeepCopyObject() runtime.Object {
	out := *s
	return &out
}

type stubClient struct {
	secrets map[types.NamespacedName]*stubSecret
}

func (s *stubClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if s.secrets == nil {
		return fmt.Errorf("not found")
	}
	if secret, exists := s.secrets[key]; exists {
		if ss, ok := obj.(*stubSecret); ok {
			*ss = *secret
		}
		return nil
	}
	return client.IgnoreNotFound(nil)
}

func (s *stubClient) List(context.Context, client.ObjectList, ...client.ListOption) error  { return nil }
func (s *stubClient) Create(context.Context, client.Object, ...client.CreateOption) error  { return nil }
func (s *stubClient) Update(context.Context, client.Object, ...client.UpdateOption) error  { return nil }
func (s *stubClient) Delete(context.Context, client.Object, ...client.DeleteOption) error  { return nil }
func (s *stubClient) DeleteAllOf(context.Context, client.Object, ...client.DeleteAllOfOption) error { return nil }
func (s *stubClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error { return nil }
func (s *stubClient) Status() client.SubResourceWriter                                      { return nil }
func (s *stubClient) Scheme() *runtime.Scheme                                             { return nil }
func (s *stubClient) RESTMapper() meta.RESTMapper                                         { return nil }
func (s *stubClient) GroupVersionKindFor(runtime.Object) (schema.GroupVersionKind, error) { return schema.GroupVersionKind{}, nil }
func (s *stubClient) SubResource(string) client.SubResourceClient { return nil }
func (s *stubClient) IsObjectNamespaced(runtime.Object) (bool, error)                     { return false, nil }

func buildResource(gvk, name, ns, uid string, obj map[string]any) *fnv1.Resource {
	res, _ := structpb.NewStruct(obj)
	return &fnv1.Resource{
		Resource: res,
	}
}

func getObj(r *fnv1.Resource) map[string]any {
	b, _ := json.Marshal(r.GetResource())
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func newXR(name, ns string, specExtra map[string]any) *fnv1.Resource {
	spec := map[string]any{
		"vmName":         "web-server-01",
		"cpu":            float64(4),
		"memory":         "8Gi",
		"image":          "quay.io/example/rhel9-web:latest",
		"namespace":      "default",
		"networkName":    "external-web-net",
		"subnet":         "10.20.30.0/24",
		"gateway":        "10.20.30.1",
		"bridgeName":     "br-ex",
		"externalIPPool": "production-external-ips",
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
		"status": map[string]any{},
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
		},
		"spec": map[string]any{
			"runStrategy": "RerunOnFailure",
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{"kubevirt.io/domain": "web-server-01"},
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

func newInfobloxCreds() *stubSecret {
	return &stubSecret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "infoblox-credentials",
			Namespace: "crossplane-system",
		},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("secret"),
			"host":     []byte("infoblox.example.com"),
		},
	}
}

func TestRunFunction(t *testing.T) {
	cases := map[string]struct {
		reason string
		req    *fnv1.RunFunctionRequest
		want   want
	}{
		"Success": {
			reason: "Should allocate IP from Infoblox and inject into cloud-init",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-success"},
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
				conditionType:   "IPInjected",
				conditionStatus: fnv1.Status_STATUS_CONDITION_TRUE,
				ipInUserData:    "10.20.30.100/24",
				dhcp4False:      true,
				gateway:         "10.20.30.1",
				vmName:          "web-server-01",
				xrExternalIP:    "10.20.30.100",
			},
		},
		"ReuseExistingIP": {
			reason: "Should reuse previously allocated IP from XR status",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-reuse-ip"},
				Observed: &fnv1.State{
					Composite: newXR("test-vm", "default", map[string]any{
						"status": map[string]any{
							"externalIP":    "10.20.30.100",
							"infobloxIPRef": "infoblox://10.20.30.100",
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
				conditionType:   "IPInjected",
				conditionStatus: fnv1.Status_STATUS_CONDITION_TRUE,
				ipInUserData:    "10.20.30.100/24",
				dhcp4False:      true,
				gateway:         "10.20.30.1",
				xrExternalIP:    "10.20.30.100",
			},
		},
		"MissingCredentials": {
			reason: "Should return false condition when Infoblox credentials secret is missing",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Tag: "test-missing-creds"},
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
				conditionType:   "IPInjected",
				conditionStatus: fnv1.Status_STATUS_CONDITION_FALSE,
				conditionMsgSub: "credentials",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stubs := &stubClient{
				secrets: map[types.NamespacedName]*stubSecret{
					{Name: "infoblox-credentials", Namespace: "crossplane-system"}: newInfobloxCreds(),
				},
			}
			if name == "MissingCredentials" {
				stubs.secrets = nil
			}

			f := &Function{
				log:          logging.NewNopLogger(),
				k8s:          stubs,
				infobloxHost: "infoblox.example.com",
				infobloxUser: "admin",
				infobloxPass: "secret",
			}
			if name != "MissingCredentials" {
				f.testAllocateIP = func(ctx context.Context, vmName, subnet, networkView, ipPool string) (string, string, error) {
					return "10.20.30.100", "infoblox://10.20.30.100", nil
				}
				f.testCreateDNSRecord = func(ctx context.Context, hostname, ip, subnet, networkView, ipPool string) error {
					return nil
				}
			}
			rsp, err := f.RunFunction(context.Background(), tc.req)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(rsp.GetConditions()) != 1 {
				t.Errorf("expected 1 condition, got %d", len(rsp.GetConditions()))
				return
			}
			c := rsp.GetConditions()[0]
			if c.GetType() != tc.want.conditionType {
				t.Errorf("condition type: want %q, got %q", tc.want.conditionType, c.GetType())
			}
			if c.GetStatus() != tc.want.conditionStatus {
				t.Errorf("condition status: want %v, got %v", tc.want.conditionStatus, c.GetStatus())
			}
			if tc.want.conditionMsgSub != "" && c.GetMessage() != "" {
				if !strings.Contains(c.GetMessage(), tc.want.conditionMsgSub) {
					t.Errorf("condition message should contain %q, got %q", tc.want.conditionMsgSub, c.GetMessage())
				}
			}

			if tc.want.conditionStatus == fnv1.Status_STATUS_CONDITION_FALSE {
				return
			}

			desired := rsp.GetDesired()
			if desired == nil || desired.GetResources() == nil {
				t.Fatal("expected desired resources, got nil")
			}

			vmRes, ok := desired.GetResources()["virtual-machine"]
			if !ok {
				t.Fatal("VM not found in desired resources")
			}
			vmObj := getObj(vmRes)
			vmSpec := vmObj["spec"].(map[string]any)
			template := vmSpec["template"].(map[string]any)
			vmSpecSpec := template["spec"].(map[string]any)
			volumes := vmSpecSpec["volumes"].([]any)

			userData := ""
			for _, v := range volumes {
				vMap := v.(map[string]any)
				if vMap["name"] == "cloudinitdisk" {
					cloudInit := vMap["cloudInitNoCloud"].(map[string]any)
					userData = cloudInit["userData"].(string)
				}
			}

			if tc.want.ipInUserData != "" && !strings.Contains(userData, tc.want.ipInUserData) {
				t.Errorf("userData should contain IP %q, got: %s", tc.want.ipInUserData, userData)
			}
			if tc.want.dhcp4False && !strings.Contains(userData, "\"dhcp4\": false") {
				t.Errorf("userData should have dhcp4: false, got: %s", userData)
			}
			if tc.want.gateway != "" && !strings.Contains(userData, tc.want.gateway) {
				t.Errorf("userData should contain gateway %q, got: %s", tc.want.gateway, userData)
			}
			if tc.want.vmName != "" && !strings.Contains(userData, "\"hostname\": \""+tc.want.vmName+"\"") {
				t.Errorf("userData should contain hostname %q, got: %s", tc.want.vmName, userData)
			}

			xr := rsp.GetDesired().GetComposite()
			if xr != nil {
				xrObj := getObj(xr)
				xrStatus := xrObj["status"].(map[string]any)

				if tc.want.xrExternalIP != "" && xrStatus["externalIP"] != tc.want.xrExternalIP {
					t.Errorf("XR externalIP: want %q, got %q", tc.want.xrExternalIP, xrStatus["externalIP"])
				}
			}
		})
	}
}

type want struct {
	conditionType     string
	conditionStatus   fnv1.Status
	conditionMsgSub   string
	ipInUserData      string
	dhcp4False        bool
	gateway           string
	vmName            string
	xrExternalIP      string
}

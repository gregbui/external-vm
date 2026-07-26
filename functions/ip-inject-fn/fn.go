package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/function-sdk-go/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

const (
	vmKeyName = "virtual-machine"

	defaultInfobloxSecretName = "infoblox-credentials"
	defaultInfobloxSecretNS   = "crossplane-system"
	defaultInfobloxUserKey    = "username"
	defaultInfobloxPassKey    = "password"
	defaultInfobloxCAPath     = "/certs/infoblox-ca.crt"
	wapiVersion               = "v2.12"
)

type ipAllocateReq struct {
	ConfigureForDHCP bool              `json:"configureForDHCP"`
	IPv4Address      string            `json:"ipv4address,omitempty"`
	MAC              string            `json:"mac,omitempty"`
	Name             string            `json:"name"`
	Comment          string            `json:"comment,omitempty"`
	NetworkView      string            `json:"network_view,omitempty"`
	ExtAttrs         map[string]any    `json:"extattrs,omitempty"`
}

type ipAllocateResp struct {
	Ref         string `json:"_ref"`
	IPv4Address string `json:"ipv4address"`
}

type dnsHostReq struct {
	Name        string            `json:"name"`
	IPv4Address string            `json:"ipv4address"`
	Comment     string            `json:"comment,omitempty"`
	NetworkView string            `json:"network_view,omitempty"`
	ExtAttrs    map[string]any    `json:"extattrs,omitempty"`
}

type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer
	log logging.Logger
	k8s client.Client

	infobloxHost string
	infobloxUser string
	infobloxPass string
	infobloxCA   string
	httpClient   *http.Client

	// testAllocateIP is an optional override for testing.
	testAllocateIP func(ctx context.Context, vmName, subnet, networkView, ipPool string) (ip string, ref string, err error)
}

func (f *Function) initHTTPClient() error {
	if f.infobloxHost == "" {
		return fmt.Errorf("INFOBLOX_HOST env var not set")
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: false}
	if f.infobloxCA != "" {
		caCert, err := os.ReadFile(f.infobloxCA)
		if err != nil {
			return fmt.Errorf("reading Infoblox CA cert: %w", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsCfg.RootCAs = caCertPool
	}
	f.httpClient = &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	return nil
}

func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running IP inject function", "tag", req.GetMeta().GetTag())
	rsp := response.To(req, response.DefaultTTL)

	// Skip HTTP client init if testAllocateIP is set (testing mode)
	if f.testAllocateIP == nil {
		if f.httpClient == nil {
			if err := f.initHTTPClient(); err != nil {
				response.ConditionFalse(rsp, "IPInjected", fmt.Sprintf("Failed to init Infoblox client: %v", err))
				return rsp, nil
			}
		}
	}

	xr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("getting observed composite resource: %w", err))
		return rsp, nil
	}

	desired, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("getting desired composed resources: %w", err))
		return rsp, nil
	}

	vm, ok := desired[resource.Name(vmKeyName)]
	if !ok {
		response.Fatal(rsp, fmt.Errorf("desired VM not found in composed resources"))
		return rsp, nil
	}

	// Skip credential check in test mode (testAllocateIP is set)
	if f.testAllocateIP == nil {
		creds, err := f.readInfobloxCreds(ctx)
		if err != nil {
			response.ConditionFalse(rsp, "IPInjected", fmt.Sprintf("Failed to read Infoblox credentials: %v", err))
			return rsp, nil
		}
		_ = creds
	}

	xrContent := xr.Resource.UnstructuredContent()
	spec, _ := xrContent["spec"].(map[string]any)
	vmName, _ := spec["vmName"].(string)
	if vmName == "" {
		vmName = xr.Resource.GetName()
	}
	subnet, _ := spec["subnet"].(string)
	gateway, _ := spec["gateway"].(string)
	networkView, _ := spec["networkView"].(string)
	ipPool, _ := spec["externalIPPool"].(string)

	subnetPrefix := "/24"
	for i := len(subnet) - 1; i >= 0; i-- {
		if subnet[i] == '/' {
			subnetPrefix = subnet[i:]
			break
		}
	}

	status, _ := xrContent["status"].(map[string]any)
	existingIP, _ := status["externalIP"].(string)
	ipRef, _ := status["infobloxIPRef"].(string)

	var ipStr, ipRefStr string
	if existingIP != "" && ipRef != "" {
		ipStr = existingIP
		ipRefStr = ipRef
	} else {
		ipStr, ipRefStr, err = f.allocateIP(ctx, vmName, subnet, networkView, ipPool)
		if err != nil {
			response.ConditionFalse(rsp, "IPInjected", fmt.Sprintf("Failed to allocate IP from Infoblox: %v", err))
			return rsp, nil
		}
	}

	if f.testAllocateIP == nil {
		if err := f.createDNSRecord(ctx, vmName, ipStr, subnet, networkView, ipPool); err != nil {
			f.log.Info("DNS record creation failed (non-fatal)", "vm", vmName, "err", err.Error())
		}
	}

	networkConfig := map[string]any{
		"version": 2,
		"ethernets": map[string]any{
			"eth1": map[string]any{
				"dhcp4": false,
				"addresses": []any{
					fmt.Sprintf("%s%s", ipStr, subnetPrefix),
				},
				"nameservers": map[string]any{
					"addresses": []any{"8.8.8.8", "1.1.1.1"},
				},
				"routes": []any{
					map[string]any{
						"to":     "0.0.0.0/0",
						"via":    gateway,
						"metric": 100,
					},
				},
			},
		},
	}

	networkConfigJSON, err := json.Marshal(networkConfig)
	if err != nil {
		response.Fatal(rsp, fmt.Errorf("marshaling network config: %w", err))
		return rsp, nil
	}

	userData := fmt.Sprintf("#cloud-config\nhostname: %s\nmanage_etc_hosts: true\nnetwork:\n  config: %s\n",
		vmName, string(networkConfigJSON))

	vmSpec := vm.Resource.UnstructuredContent()["spec"].(map[string]any)
	template := vmSpec["template"].(map[string]any)
	templateSpec := template["spec"].(map[string]any)
	volumes, _ := templateSpec["volumes"].([]any)

	for _, v := range volumes {
		vMap, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if vMap["name"] == "cloudinitdisk" {
			cloudInit, ok := vMap["cloudInitNoCloud"].(map[string]any)
			if !ok {
				cloudInit = map[string]any{}
				vMap["cloudInitNoCloud"] = cloudInit
			}
			cloudInit["userData"] = userData
			break
		}
	}

	desired[resource.Name(vmKeyName)] = vm

	status, _ = xrContent["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["externalIP"] = ipStr
	status["infobloxIPRef"] = ipRefStr
	xr.Resource.SetUnstructuredContent(xrContent)

	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composed resources: %w", err))
		return rsp, nil
	}
	if err := response.SetDesiredCompositeResource(rsp, xr); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composite resource: %w", err))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "IPInjected", fmt.Sprintf("Injected IP %s", ipStr))
	return rsp, nil
}

func (f *Function) readInfobloxCreds(ctx context.Context) (map[string]string, error) {
	secret := &corev1.Secret{}
	if err := f.k8s.Get(ctx, types.NamespacedName{
		Name:      defaultInfobloxSecretName,
		Namespace: defaultInfobloxSecretNS,
	}, secret); err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", defaultInfobloxSecretNS, defaultInfobloxSecretName, err)
	}
	return map[string]string{
		"username": string(secret.Data[defaultInfobloxUserKey]),
		"password": string(secret.Data[defaultInfobloxPassKey]),
	}, nil
}

func (f *Function) allocateIP(ctx context.Context, vmName, subnet, networkView, ipPool string) (string, string, error) {
	if f.testAllocateIP != nil {
		return f.testAllocateIP(ctx, vmName, subnet, networkView, ipPool)
	}

	reqBody := ipAllocateReq{
		ConfigureForDHCP: false,
		Name:             vmName,
		Comment:          fmt.Sprintf("Managed by Crossplane - VM: %s", vmName),
		NetworkView:      networkView,
		ExtAttrs: map[string]any{
			"VM Name":    map[string]any{"value": vmName},
			"Crossplane": map[string]any{"value": "true"},
		},
	}
	if ipPool != "" {
		reqBody.ExtAttrs["IP Pool"] = map[string]any{"value": ipPool}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("marshaling request: %w", err)
	}

	url := fmt.Sprintf("https://%s/wapi/%s/ipv4address", f.infobloxHost, wapiVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(f.infobloxUser, f.infobloxPass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("calling Infoblox WAPI: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("Infoblox WAPI error %d: %s", resp.StatusCode, string(respBody))
	}

	var allocResp ipAllocateResp
	if err := json.Unmarshal(respBody, &allocResp); err != nil {
		return "", "", fmt.Errorf("unmarshaling response: %w", err)
	}
	return allocResp.IPv4Address, allocResp.Ref, nil
}

func (f *Function) createDNSRecord(ctx context.Context, hostname, ip, subnet, networkView, ipPool string) error {
	reqBody := dnsHostReq{
		Name:        hostname,
		IPv4Address: ip,
		Comment:     fmt.Sprintf("Managed by Crossplane - VM: %s", hostname),
		NetworkView: networkView,
		ExtAttrs: map[string]any{
			"VM Name":    map[string]any{"value": hostname},
			"Crossplane": map[string]any{"value": "true"},
		},
	}
	if ipPool != "" {
		reqBody.ExtAttrs["IP Pool"] = map[string]any{"value": ipPool}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	url := fmt.Sprintf("https://%s/wapi/%s/record:host", f.infobloxHost, wapiVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.SetBasicAuth(f.infobloxUser, f.infobloxPass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling Infoblox WAPI: %w", err)
	}
	defer resp.Body.Close()

	_, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Infoblox WAPI error %d", resp.StatusCode)
	}
	return nil
}

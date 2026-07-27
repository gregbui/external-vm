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

	"encoding/base64"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	vmKeyName = "virtual-machine"

	defaultInfobloxSecretName = "infoblox-credentials"
	defaultInfobloxSecretNS   = "crossplane-system"
	defaultInfobloxUserKey    = "username"
	defaultInfobloxPassKey    = "password"
	defaultInfobloxCAPath     = "/certs/infoblox-ca.crt"
	wapiVersion               = "v2.12"

	// SysPrep ISO constants
	sysprepISOSize       = 16 * 1024 * 1024 // 16 MiB
	sysprepISOFileName   = "unattend.xml"
	sysprepSecretNameFmt = "%s-sysprep-iso"
)

type ipAllocateReq struct {
	ConfigureForDHCP bool           `json:"configureForDHCP"`
	IPv4Address      string         `json:"ipv4address,omitempty"`
	MAC              string         `json:"mac,omitempty"`
	Name             string         `json:"name"`
	Comment          string         `json:"comment,omitempty"`
	NetworkView      string         `json:"network_view,omitempty"`
	ExtAttrs         map[string]any `json:"extattrs,omitempty"`
}

type ipAllocateResp struct {
	Ref         string `json:"_ref"`
	IPv4Address string `json:"ipv4address"`
}

type dnsHostReq struct {
	Name        string         `json:"name"`
	IPv4Address string         `json:"ipv4address"`
	Comment     string         `json:"comment,omitempty"`
	NetworkView string         `json:"network_view,omitempty"`
	ExtAttrs    map[string]any `json:"extattrs,omitempty"`
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
	// testCreateDNSRecord is an optional override for testing.
	testCreateDNSRecord func(ctx context.Context, hostname, ip, subnet, networkView, ipPool string) error
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

	// ── Extract osType ──
	osType, _ := spec["osType"].(string)
	if osType == "" {
		osType = "linux"
	}

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

	// Create DNS record
	hostname := fmt.Sprintf("%s.%s", vmName, subnet[:len(subnet)-len(subnetPrefix)+1])
	if err := f.createDNSRecord(ctx, hostname, ipStr, subnet, networkView, ipPool); err != nil {
		f.log.Info("Failed to create DNS record", "error", err)
		// Non-fatal — IP is still allocated
	}

	// ── Inject network config based on osType ──
	if osType == "windows" {
		if err := f.injectSysPrep(ctx, desired, xr, vmName, ipStr, subnet, subnetPrefix, gateway); err != nil {
			response.ConditionFalse(rsp, "IPInjected", fmt.Sprintf("Failed to inject SysPrep config: %v", err))
			return rsp, nil
		}
	} else {
		// Linux: existing cloud-init behavior
		userData := f.buildCloudInitUserData(vmName, ipStr, subnet, subnetPrefix, gateway)
		if err := f.injectCloudInitConfig(vm, userData); err != nil {
			response.ConditionFalse(rsp, "IPInjected", fmt.Sprintf("Failed to inject cloud-init config: %v", err))
			return rsp, nil
		}
	}

	desired[resource.Name(vmKeyName)] = vm

	status, _ = xrContent["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	status["externalIP"] = ipStr
	status["infobloxIPRef"] = ipRefStr
	status["osType"] = osType
	xr.Resource.SetUnstructuredContent(xrContent)

	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composed resources: %w", err))
		return rsp, nil
	}
	if err := response.SetDesiredCompositeResource(rsp, xr); err != nil {
		response.Fatal(rsp, fmt.Errorf("setting desired composite resource: %w", err))
		return rsp, nil
	}

	response.ConditionTrue(rsp, "IPInjected", fmt.Sprintf("Injected IP %s [os=%s]", ipStr, osType))
	return rsp, nil
}

// ── Linux: cloud-init ──

func (f *Function) buildCloudInitUserData(vmName, ip, subnet, subnetPrefix, gateway string) string {
	networkConfig := map[string]any{
		"network": map[string]any{
			"config": []any{
				map[string]any{
					"type":        "physical",
					"name":        "eth1",
					"subnets":     []any{map[string]any{"address": ip + subnetPrefix, "dns_nameservers": []any{"8.8.8.8", "1.1.1.1"}, "routes": []any{map[string]any{"to": "0.0.0.0/0", "via": gateway, "metric": 100}}}},
					"dhcp4":       false,
					"dhcp6":       false,
					"mac_address": "",
				},
			},
		},
		"hostname":   vmName,
		"fqdn":       fmt.Sprintf("%s.local", vmName),
		"users":      []any{map[string]any{"name": "root", "ssh_authorized_keys": []any{}}},
		"ssh_pwauth": true,
	}

	networkConfigJSON, err := json.MarshalIndent(networkConfig, "", "  ")
	if err != nil {
		return fmt.Sprintf("#cloud-config\nhostname: %s\n", vmName)
	}

	return fmt.Sprintf("#cloud-config\n%s", string(networkConfigJSON))
}

func (f *Function) injectCloudInitConfig(vm *resource.DesiredComposed, userData string) error {
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

	return nil
}

// ── Windows: SysPrep ──

func (f *Function) injectSysPrep(ctx context.Context, desired map[resource.Name]*resource.DesiredComposed, xr *resource.Composite, vmName, ip, subnet, subnetPrefix, gateway string) error {
	// Generate unattend.xml
	unattendXML, err := f.generateUnattendXML(vmName, ip, subnet, subnetPrefix, gateway)
	if err != nil {
		return fmt.Errorf("generating unattend.xml: %w", err)
	}

	// Create ISO with unattend.xml
	isoBytes, err := f.createSysPrepISO(unattendXML)
	if err != nil {
		return fmt.Errorf("creating SysPrep ISO: %w", err)
	}

	// Create Secret with ISO
	namespace := xr.Resource.GetNamespace()
	isoSecretName := fmt.Sprintf(sysprepSecretNameFmt, vmName)

	isoSecret := resource.NewDesiredComposed()
	isoSecret.Resource.SetAPIVersion("v1")
	isoSecret.Resource.SetKind("Secret")
	isoSecret.Resource.SetName(isoSecretName)
	isoSecret.Resource.SetNamespace(namespace)
	isoSecret.Resource.SetLabels(map[string]string{
		"external-vm-fn": "true",
	})
	isoSecret.Resource.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         xr.Resource.GetAPIVersion(),
			Kind:               xr.Resource.GetKind(),
			Name:               xr.Resource.GetName(),
			UID:                xr.Resource.GetUID(),
			Controller:         boolPtr(true),
			BlockOwnerDeletion: boolPtr(true),
		},
	})

	isoSecret.Resource.Object["data"] = map[string]any{
		"sysprep.iso": base64.StdEncoding.EncodeToString(isoBytes),
	}
	isoSecret.Resource.Object["type"] = "Opaque"

	desired[resource.Name("sysprep-iso")] = isoSecret

	// Patch VM: add ISO disk and volume
	vm, ok := desired[resource.Name(vmKeyName)]
	if !ok {
		return fmt.Errorf("VM not found in desired resources")
	}
	vmSpec := vm.Resource.UnstructuredContent()["spec"].(map[string]any)
	template := vmSpec["template"].(map[string]any)
	templateSpec := template["spec"].(map[string]any)
	volumes, _ := templateSpec["volumes"].([]any)

	// Add sysprep ISO volume
	volumes = append(volumes, map[string]any{
		"name": "sysprepiso",
		"disk": map[string]any{
			"image": fmt.Sprintf("docker://localhost/%s/%s:latest", namespace, isoSecretName),
		},
	})

	// Find cloudinitdisk and add sysprepiso after it
	newVolumes := []any{}
	for _, v := range volumes {
		newVolumes = append(newVolumes, v)
		if vMap, ok := v.(map[string]any); ok {
			if name, ok := vMap["name"].(string); ok && name == "cloudinitdisk" {
				// Insert sysprep ISO volume after cloudinitdisk
				newVolumes = append(newVolumes, volumes[len(volumes)-1])
				volumes = newVolumes
				break
			}
		}
	}

	templateSpec["volumes"] = volumes

	// Add sysprep disk to devices
	domain := templateSpec["domain"].(map[string]any)
	devices := domain["devices"].(map[string]any)
	disks, _ := devices["disks"].([]any)
	if disks == nil {
		disks = []any{}
	}
	disks = append(disks, map[string]any{
		"name": "sysprepiso",
		"disk": map[string]any{
			"bus": "sata",
		},
	})
	devices["disks"] = disks

	return nil
}

// generateUnattendXML creates a SysPrep unattend.xml with network configuration.
func (f *Function) generateUnattendXML(vmName, ip, subnet, subnetPrefix, gateway string) (string, error) {
	unattend := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="specialize">
    <component name="Microsoft-Windows-TCPIP" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
      <Interfaces>
        <Interface id="eth1">
          <Ipv4Addresses>
            <IpAddress id="0" wcm:action="add" wcm:keyValue="1">%s%s</IpAddress>
          </Ipv4Addresses>
          <Routes>
            <Route id="0" wcm:action="add" wcm:keyValue="1">
              <Identifier>0</Identifier>
              <Prefix>0.0.0.0/0</Prefix>
              <NextHopAddress>%s</NextHopAddress>
            </Route>
          </Routes>
          <EnableDHCP>false</EnableDHCP>
        </Interface>
      </Interfaces>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
      <ComputerName>%s</ComputerName>
      <ProductKey></ProductKey>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
      <UserAccounts>
        <LocalAccounts>
          <LocalAccount wcm:action="add">
            <Name>Administrator</Name>
            <Password>
              <Value>Pa$$w0rd123!</Value>
              <PlainText>true</PlainText>
            </Password>
          </LocalAccount>
        </LocalAccounts>
      </UserAccounts>
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <ProtectYourPC>1</ProtectYourPC>
        <SkipUserOOBE>true</SkipUserOOBE>
        <SkipMachineOOBE>false</SkipMachineOOBE>
      </OOBE>
    </component>
  </settings>
</unattend>`, ip, subnetPrefix, gateway, vmName)

	return unattend, nil
}

// createSysPrepISO creates an ISO9660 filesystem containing the unattend.xml file.
func (f *Function) createSysPrepISO(unattendXML string) ([]byte, error) {
	blocksize := int64(2048)
	size := int64(sysprepISOSize)

	// Create a temp file for the ISO
	tmpFile, err := os.CreateTemp("", "sysprep-*.iso")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Create backend from file
	b := file.New(tmpFile, false)

	// Create ISO9660 filesystem
	fs, err := iso9660.Create(b, size, 2048*blocksize, blocksize, "")
	if err != nil {
		return nil, fmt.Errorf("creating ISO9660 filesystem: %w", err)
	}

	// Write unattend.xml to the root of the ISO
	isofile, err := fs.OpenFile(sysprepISOFileName, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("creating unattend.xml in ISO: %w", err)
	}
	if _, err := isofile.Write([]byte(unattendXML)); err != nil {
		isofile.Close()
		return nil, fmt.Errorf("writing unattend.xml to ISO: %w", err)
	}
	isofile.Close()

	// Finalize with Rock Ridge extensions
	err = fs.Finalize(iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: "SYSPREP",
	})
	if err != nil {
		return nil, fmt.Errorf("finalizing ISO: %w", err)
	}

	// Read ISO bytes from temp file
	tmpFile.Seek(0, 0)
	isoBytes, err := io.ReadAll(tmpFile)
	if err != nil {
		return nil, fmt.Errorf("reading ISO from temp file: %w", err)
	}

	return isoBytes, nil
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
	if f.testCreateDNSRecord != nil {
		return f.testCreateDNSRecord(ctx, hostname, ip, subnet, networkView, ipPool)
	}
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

func boolPtr(b bool) *bool { return &b }

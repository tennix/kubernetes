package ucloud

import (
	"errors"
	"io"

	"github.com/golang/glog"
	gcfg "gopkg.in/gcfg.v1"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/intstr"
)

const (
	providerName = "ucloud"
	operatorName = "Bgp"
)

var (
	ULBNotFound = errors.New("ulb not found")
	EIPNotFound = errors.New("eip not found")
)

type CloudConfig struct {
	Global struct {
		ApiURL     string `gcfg:"api-url"`
		PublicKey  string `gcfg:"public-key"`
		PrivateKey string `gcfg:"private-key"`
		Region     string `gcfg:"region"`
		Zone       string `gcfg:"zone"`
		ProjectID  string `gcfg:"project-id"`
	}
}

type Cloud struct {
	UClient
	Region    string
	Zone      string
	ProjectID string
	// UHost instance that we're running on
	selfInstance *UHostInstanceSet
}

func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return c, true
}

func (c *Cloud) Instances() (cloudprovider.Instances, bool) {
	return c, true
}

func (c *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

func (c *Cloud) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

func (c *Cloud) ProviderName() string {
	return providerName
}

func (c *Cloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

func (c *Cloud) Zones() (cloudprovider.Zones, bool) {
	return c, true
}

func (c *Cloud) GetZone() (cloudprovider.Zone, error) {
	return cloudprovider.Zone{
		FailureDomain: c.Zone,
		Region:        c.Region,
	}, nil
}

// NodeAddresses implement cloudprovider.Instances interface
func (c *Cloud) NodeAddresses(nodeName types.NodeName) ([]api.NodeAddress, error) {
	addrs := []api.NodeAddress{
		api.NodeAddress{
			Type:    "HostName",
			Address: string(nodeName),
		},
	}
	return addrs, nil
}

// ExternalID implement cloudprovider.Instances interface
func (c *Cloud) ExternalID(nodeName types.NodeName) (string, error) {
	ids, err := c.getUHostIDs([]string{string(nodeName)})
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", cloudprovider.InstanceNotFound
	}
	return ids[0], nil
}

// InstanceID implement cloudprovider.Instances interface
func (c *Cloud) InstanceID(nodeName types.NodeName) (string, error) {
	ids, err := c.getUHostIDs([]string{string(nodeName)})
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", cloudprovider.InstanceNotFound
	}
	return ids[0], nil
}

// InstanceType implement cloudprovider.Instances interface
func (c *Cloud) InstanceType(nodeName types.NodeName) (string, error) {
	p := DescribeUHostInstanceParam{
		Region:    c.Region,
		ProjectID: c.ProjectID,
		Limit:     100,
	}
	r, err := c.UClient.DescribeUHostInstance(p)
	if err != nil {
		return "", err
	}
	if r.RetCode != 0 {
		return "", errors.New(r.Message)
	}
	for _, host := range r.UHostSet {
		for _, ip := range host.IPSet {
			if ip.IP == string(nodeName) {
				return host.UHostType, nil
			}
		}
	}
	return "", cloudprovider.InstanceNotFound
}

// List implement cloudprovider.Instances interface
func (c *Cloud) List(filter string) ([]types.NodeName, error) {
	return nil, errors.New("unimplemented")
}

// AddSSHKeyToAllInstances implement cloudprovider.Instances interface
func (c *Cloud) AddSSHKeyToAllInstances(user string, keyData []byte) error {
	return errors.New("unimplemented")
}

// CurrentNodeName implement cloudprovider.Instances interface
func (c *Cloud) CurrentNodeName(hostname string) (types.NodeName, error) {
	return types.NodeName(hostname), nil
}

func (c *Cloud) describeLoadBalancer(name string) (*ULBSet, error) {
	p := DescribeULBParam{
		Region:    c.Region,
		ProjectID: c.ProjectID,
		Limit:     100,
	}
	resp, err := c.UClient.DescribeULB(p)
	glog.V(3).Infof("describe ULB response: %+v", resp)
	if err != nil {
		return nil, err
	}
	if resp.RetCode != 0 {
		return nil, ULBNotFound
	}
	for _, lb := range resp.DataSet {
		if lb.Name == name {
			return &lb, nil
		}
	}
	return nil, ULBNotFound
}

func (c *Cloud) createLoadBalancer(name string, hostIDs []string, backendPort int, frontendPort int) (string, error) {
	eip := ""
	p1 := CreateULBParam{
		Region:    c.Region,
		ProjectID: c.ProjectID,
		ULBName:   name,
		Tag:       "tidb-k8s-poc",
		OuterMode: "Yes",
	}
	r1, err := c.UClient.CreateULB(p1)
	glog.V(3).Infof("create ULB response: %+v", r1)
	if err != nil {
		return eip, err
	}
	if r1.RetCode != 0 {
		return eip, errors.New(r1.Message)
	}

	p2 := AllocateEIPParam{
		Region:       c.Region,
		OperatorName: operatorName,
		Bandwidth:    2,
		Quantity:     1,
	}
	r2, err := c.UClient.AllocateEIP(p2)
	if err != nil {
		return eip, err
	}
	glog.V(3).Infof("allocate EIP response: %+v", r2)
	if r2.RetCode != 0 {
		return eip, errors.New(r2.Message)
	}

	p3 := BindEIPParam{
		Region:       c.Region,
		EIPID:        r2.EIPSet[0].EIPID,
		ResourceType: "ulb",
		ResourceID:   r1.ULBID,
	}
	r3, err := c.UClient.BindEIP(p3)
	if err != nil {
		return eip, err
	}
	glog.V(3).Infof("bind EIP response: %+v", r3)
	if r3.RetCode != 0 {
		return eip, errors.New(r3.Message)
	}

	p4 := CreateVServerParam{
		Region:        c.Region,
		ProjectID:     c.ProjectID,
		ULBID:         r1.ULBID,
		VServerName:   "tidb-server",
		Protocol:      "TCP",
		FrontendPort:  frontendPort,
		ListenType:    "RequestProxy",
		ClientTimeout: 60,
	}
	r4, err := c.UClient.CreateVServer(p4)
	if err != nil {
		return eip, err
	}
	glog.V(3).Infof("create vserver response: %+v", r4)
	if r4.RetCode != 0 {
		return eip, errors.New(r4.Message)
	}

	for _, host := range hostIDs {
		p5 := AllocateBackendParam{
			Region:       c.Region,
			ProjectID:    c.ProjectID,
			ULBID:        r1.ULBID,
			VServerID:    r4.VServerID,
			ResourceType: "UHost",
			ResourceID:   host,
			Port:         backendPort,
			Enabled:      1,
		}
		r5, err := c.UClient.AllocateBackend(p5)
		if err != nil {
			return eip, err
		}
		if r5.RetCode != 0 {
			return eip, errors.New(r5.Message)
		}
	}

	eip = r2.EIPSet[0].EIPAddr[0].IP
	return eip, nil
}

func (c *Cloud) deleteLoadBalancer(name string) error {
	ulbSet, err := c.describeLoadBalancer(name)
	if err != nil && err == ULBNotFound {
		return nil
	}
	p := DeleteULBParam{
		Region:    c.Region,
		ProjectID: c.ProjectID,
		ULBID:     ulbSet.ULBID,
	}
	r, err := c.UClient.DeleteULB(p)
	if err != nil {
		glog.Error(err)
	}
	if r.RetCode != 0 {
		glog.Error(r.Message)
	}
	for _, ipSet := range ulbSet.IPSet {
		p2 := ReleaseEIPParam{
			Region: c.Region,
			EIPID:  ipSet.EIPID,
		}
		r2, err := c.UClient.ReleaseEIP(p2)
		glog.V(3).Infof("release eip %s response: %+v", ipSet.EIPID, r2)
		if err != nil {
			glog.Error(err)
			continue
		}
		if r2.RetCode != 0 {
			glog.Error(r2.Message)
		}
	}
	return nil
}

func (c *Cloud) GetLoadBalancer(clusterName string, service *api.Service) (status *api.LoadBalancerStatus, exists bool, err error) {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(3).Infof("get loadbalancer name: %s", loadBalancerName)
	ulbSet, err := c.describeLoadBalancer(loadBalancerName)
	if err != nil {
		return nil, false, err
	}
	status, err = toLBStatus(ulbSet)
	if err != nil {
		return nil, false, err
	}
	return status, true, nil
}

func toLBStatus(ulbSet *ULBSet) (*api.LoadBalancerStatus, error) {
	if len(ulbSet.IPSet) == 0 {
		return nil, EIPNotFound
	}
	ing := api.LoadBalancerIngress{IP: ulbSet.IPSet[0].EIP}
	return &api.LoadBalancerStatus{Ingress: []api.LoadBalancerIngress{ing}}, nil
}

func (c *Cloud) EnsureLoadBalancer(clusterName string, service *api.Service, nodeNames []string) (*api.LoadBalancerStatus, error) {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(3).Infof("loadBalancer name: %s", loadBalancerName)
	_, err := c.describeLoadBalancer(loadBalancerName)
	if err != nil && err != ULBNotFound {
		return nil, err
	}
	if len(service.Spec.Ports) == 0 {
		return nil, errors.New("no port found for service")
	}
	backendPort := int(service.Spec.Ports[0].NodePort)
	frontendPort := int(service.Spec.Ports[0].Port)
	if service.Spec.Ports[0].TargetPort.Type == intstr.Int {
		frontendPort = int(service.Spec.Ports[0].TargetPort.IntVal)
	}
	uHostIDs, err := c.getUHostIDs(nodeNames)
	if err != nil {
		return nil, err
	}
	eip, err := c.createLoadBalancer(loadBalancerName, uHostIDs, backendPort, frontendPort)
	if err != nil {
		return nil, err
	}
	status := &api.LoadBalancerStatus{
		Ingress: []api.LoadBalancerIngress{
			api.LoadBalancerIngress{IP: eip},
		},
	}
	return status, nil
}

func (c *Cloud) getUHostIDs(nodeNames []string) ([]string, error) { // nodeName is node IP
	var instanceIDs []string
	p := DescribeUHostInstanceParam{
		Region:    c.Region,
		ProjectID: c.ProjectID,
		Limit:     100,
	}
	r, err := c.UClient.DescribeUHostInstance(p)
	if err != nil {
		return instanceIDs, err
	}
	if r.RetCode != 0 {
		return instanceIDs, errors.New(r.Message)
	}
	nn := make(map[string]bool)
	for _, name := range nodeNames {
		nn[name] = true
	}
	for _, host := range r.UHostSet {
		for _, ip := range host.IPSet {
			if _, ok := nn[ip.IP]; ok {
				instanceIDs = append(instanceIDs, host.UHostID)
			}
		}
	}
	return instanceIDs, nil
}

func (c *Cloud) UpdateLoadBalancer(clusterName string, service *api.Service, nodeNames []string) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(3).Infof("update loadbalancer name: %s", loadBalancerName)
	ulbSet, err := c.describeLoadBalancer(loadBalancerName)
	if err != nil {
		return err
	}
	if len(service.Spec.Ports) == 0 {
		return errors.New("no port found for serivce")
	}
	port := int(service.Spec.Ports[0].Port)
	uHostIDs, err := c.getUHostIDs(nodeNames)
	if err != nil {
		return err
	}
	ulbID := ulbSet.ULBID
	vserverID := ulbSet.VServerSet[0].VserverID
	m1 := make(map[string]bool)
	m2 := make(map[string]string)
	for _, host := range uHostIDs {
		m1[host] = true
	}
	for _, backend := range ulbSet.VServerSet[0].VServerSet {
		m2[backend.ResourceID] = backend.BackendID
	}
	// remove backend if not in m1, create backend if not in m1
	for host, backendID := range m2 {
		if _, ok := m1[host]; !ok {
			// delete backend
			p := ReleaseBackendParam{
				Region:    c.Region,
				ProjectID: c.ProjectID,
				ULBID:     ulbID,
				BackendID: backendID,
			}
			r, err := c.UClient.ReleaseBackend(p)
			if err != nil {
				return err
			}
			glog.V(3).Infof("update loadbalancer(release backend) response: %+v", r)
			if r.RetCode != 0 {
				return errors.New(r.Message)
			}
		}
	}
	for host := range m1 {
		if _, ok := m2[host]; !ok {
			// create backend
			p := AllocateBackendParam{
				Region:       c.Region,
				ProjectID:    c.ProjectID,
				ULBID:        ulbID,
				VServerID:    vserverID,
				ResourceType: "UHost",
				ResourceID:   host,
				Port:         port,
				Enabled:      1,
			}
			r, err := c.UClient.AllocateBackend(p)
			if err != nil {
				return err
			}
			glog.V(3).Infof("update loadbalancer(allocate backend) response: %+v", r)
			if r.RetCode != 0 {
				return errors.New(r.Message)
			}
		}
	}
	return nil
}

func (c *Cloud) EnsureLoadBalancerDeleted(clusterName string, service *api.Service) error {
	loadBalancerName := cloudprovider.GetLoadBalancerName(service)
	glog.V(3).Infof("loadbalancer name: %s", loadBalancerName)
	return c.deleteLoadBalancer(loadBalancerName)
}

func newUCloud(config io.Reader) (*Cloud, error) {
	var (
		cfg CloudConfig
		err error
	)
	err = gcfg.ReadInto(&cfg, config)
	if err != nil {
		return nil, err
	}
	cloud := &Cloud{
		Region:    cfg.Global.Region,
		Zone:      cfg.Global.Zone,
		ProjectID: cfg.Global.ProjectID,
	}
	cloud.UClient = UClient{
		PrivateKey: cfg.Global.PrivateKey,
		PublicKey:  cfg.Global.PublicKey,
		BaseURL:    cfg.Global.ApiURL,
	}
	glog.V(3).Infof("ucloud: %+v", cloud)
	return cloud, nil
}

func init() {
	cloudprovider.RegisterCloudProvider(providerName, func(config io.Reader) (cloudprovider.Interface, error) {
		return newUCloud(config)
	})
}

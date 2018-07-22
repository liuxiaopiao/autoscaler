/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package oke

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"gopkg.in/gcfg.v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
)

const (
	refreshInterval = 1 * time.Minute
	// The path of deployment parameters for standard vm.
	deploymentParametersPath = "/var/lib/oke/okedeploy.parameters.json"
)

// OKEManager handles OKE communication and data caching.
type OKEManager struct {
	config                *Config
	lastRefresh           time.Time
	asgAutoDiscoverySpecs []cloudprovider.LabelAutoDiscoveryConfig
	explicitlyConfigured  map[string]bool
}

// Config holds the configuration parsed from the --cloud-config flag
type Config struct {
	TenancyOCID       string `json:"tenantId" yaml:"tenantId"`
	UserOCID string `json:"userOCID" yaml:"userOCID"`
	KeyFingerprint                 string `json:"keyFingerprint" yaml:"keyFingerprint"`
	Region             string `json:"region" yaml:"region"`
	PrivateKey         string `json:"privateKey" yaml:"privateKey"`
	PrivateKeyPassphrase  string `json:"privateKeyPassphrase" yaml:"privateKeyPassphrase"`
	ClusterName string `json:"clusterName" yaml:"clusterName"`
}

// TrimSpace removes all leading and trailing white spaces.
func (c *Config) TrimSpace() {
	c.TenancyOCID = strings.TrimSpace(c.TenancyOCID)
	c.UserOCID = strings.TrimSpace(c.UserOCID)
	c.KeyFingerprint = strings.TrimSpace(c.KeyFingerprint)
	c.Region = strings.TrimSpace(c.Region)
	c.ClusterName = strings.TrimSpace(c.ClusterName)
}

// CreateOKEManager creates OKE Manager object to work with OKE.
func CreateOKEManager(configReader io.Reader, discoveryOpts cloudprovider.NodeGroupDiscoveryOptions) (*OKEManager, error) {
	var err error
	var cfg Config

	if configReader != nil {
		if err := gcfg.ReadInto(&cfg, configReader); err != nil {
			glog.Errorf("Couldn't read config: %v", err)
			return nil, err
		}
	} else {
		cfg.Cloud = os.Getenv("ARM_CLOUD")
		cfg.SubscriptionID = os.Getenv("ARM_SUBSCRIPTION_ID")
		cfg.ResourceGroup = os.Getenv("ARM_RESOURCE_GROUP")
		cfg.TenantID = os.Getenv("ARM_TENANT_ID")
		cfg.AADClientID = os.Getenv("ARM_CLIENT_ID")
		cfg.AADClientSecret = os.Getenv("ARM_CLIENT_SECRET")
		cfg.VMType = strings.ToLower(os.Getenv("ARM_VM_TYPE"))
		cfg.AADClientCertPath = os.Getenv("ARM_CLIENT_CERT_PATH")
		cfg.AADClientCertPassword = os.Getenv("ARM_CLIENT_CERT_PASSWORD")
		cfg.Deployment = os.Getenv("ARM_DEPLOYMENT")
		cfg.ClusterName = os.Getenv("AZURE_CLUSTER_NAME")
		cfg.NodeResourceGroup = os.Getenv("AZURE_NODE_RESOURCE_GROUP")

		useManagedIdentityExtensionFromEnv := os.Getenv("ARM_USE_MANAGED_IDENTITY_EXTENSION")
		if len(useManagedIdentityExtensionFromEnv) > 0 {
			cfg.UseManagedIdentityExtension, err = strconv.ParseBool(useManagedIdentityExtensionFromEnv)
			if err != nil {
				return nil, err
			}
		}
	}
	cfg.TrimSpace()

	// Defaulting vmType to vmss.
	if cfg.VMType == "" {
		cfg.VMType = vmTypeVMSS
	}

	// Read parameters from deploymentParametersPath if it is not set.
	if cfg.VMType == vmTypeStandard && len(cfg.DeploymentParameters) == 0 {
		parameters, err := readDeploymentParameters(deploymentParametersPath)
		if err != nil {
			glog.Errorf("readDeploymentParameters failed with error: %v", err)
			return nil, err
		}

		cfg.DeploymentParameters = parameters
	}

	// Defaulting env to OKE Public Cloud.
	env := oke.PublicCloud
	if cfg.Cloud != "" {
		env, err = oke.EnvironmentFromName(cfg.Cloud)
		if err != nil {
			return nil, err
		}
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	glog.Infof("Starting oke manager with subscription ID %q", cfg.SubscriptionID)

	azClient, err := newAzClient(&cfg, &env)
	if err != nil {
		return nil, err
	}

	// Create oke manager.
	manager := &OKEManager{
		config:               &cfg,
		env:                  env,
		azClient:             azClient,
		explicitlyConfigured: make(map[string]bool),
	}

	cache, err := newAsgCache()
	if err != nil {
		return nil, err
	}
	manager.asgCache = cache

	specs, err := discoveryOpts.ParseLabelAutoDiscoverySpecs()
	if err != nil {
		return nil, err
	}
	manager.asgAutoDiscoverySpecs = specs

	if err := manager.fetchExplicitAsgs(discoveryOpts.NodeGroupSpecs); err != nil {
		return nil, err
	}

	if err := manager.forceRefresh(); err != nil {
		return nil, err
	}

	return manager, nil
}

func (m *OKEManager) fetchExplicitAsgs(specs []string) error {
	changed := false
	for _, spec := range specs {
		asg, err := m.buildAsgFromSpec(spec)
		if err != nil {
			return fmt.Errorf("failed to parse node group spec: %v", err)
		}
		if m.RegisterAsg(asg) {
			changed = true
		}
		m.explicitlyConfigured[asg.Id()] = true
	}

	if changed {
		if err := m.regenerateCache(); err != nil {
			return err
		}
	}
	return nil
}

func (m *OKEManager) buildAsgFromSpec(spec string) (cloudprovider.NodeGroup, error) {
	scaleToZeroSupported := scaleToZeroSupportedStandard
	if strings.EqualFold(m.config.VMType, vmTypeVMSS) {
		scaleToZeroSupported = scaleToZeroSupportedVMSS
	}
	s, err := dynamic.SpecFromString(spec, scaleToZeroSupported)
	if err != nil {
		return nil, fmt.Errorf("failed to parse node group spec: %v", err)
	}

	return NewContainerServiceAgentPool(s, m)

}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (m *OKEManager) Refresh() error {
	if m.lastRefresh.Add(refreshInterval).After(time.Now()) {
		return nil
	}
	return m.forceRefresh()
}

func (m *OKEManager) forceRefresh() error {
	if err := m.fetchAutoAsgs(); err != nil {
		glog.Errorf("Failed to fetch ASGs: %v", err)
		return err
	}
	m.lastRefresh = time.Now()
	glog.V(2).Infof("Refreshed ASG list, next refresh after %v", m.lastRefresh.Add(refreshInterval))
	return nil
}

// Fetch automatically discovered ASGs. These ASGs should be unregistered if
// they no longer exist in OKE.
func (m *OKEManager) fetchAutoAsgs() error {
	groups, err := m.getFilteredAutoscalingGroups(m.asgAutoDiscoverySpecs)
	if err != nil {
		return fmt.Errorf("cannot autodiscover ASGs: %s", err)
	}

	changed := false
	exists := make(map[string]bool)
	for _, asg := range groups {
		asgID := asg.Id()
		exists[asgID] = true
		if m.explicitlyConfigured[asgID] {
			// This ASG was explicitly configured, but would also be
			// autodiscovered. We want the explicitly configured min and max
			// nodes to take precedence.
			glog.V(3).Infof("Ignoring explicitly configured ASG %s for autodiscovery.", asg.Id())
			continue
		}
		if m.RegisterAsg(asg) {
			glog.V(3).Infof("Autodiscovered ASG %s using tags %v", asg.Id(), m.asgAutoDiscoverySpecs)
			changed = true
		}
	}

	for _, asg := range m.getAsgs() {
		asgID := asg.Id()
		if !exists[asgID] && !m.explicitlyConfigured[asgID] {
			m.UnregisterAsg(asg)
			changed = true
		}
	}

	if changed {
		if err := m.regenerateCache(); err != nil {
			return err
		}
	}

	return nil
}

func (m *OKEManager) getAsgs() []cloudprovider.NodeGroup {
	return m.asgCache.get()
}

// RegisterAsg registers an ASG.
func (m *OKEManager) RegisterAsg(asg cloudprovider.NodeGroup) bool {
	return m.asgCache.Register(asg)
}

// UnregisterAsg unregisters an ASG.
func (m *OKEManager) UnregisterAsg(asg cloudprovider.NodeGroup) bool {
	return m.asgCache.Unregister(asg)
}

// GetAsgForInstance returns AsgConfig of the given Instance
func (m *OKEManager) GetAsgForInstance(instance *okeRef) (cloudprovider.NodeGroup, error) {
	return m.asgCache.FindForInstance(instance, m.config.VMType)
}

func (m *OKEManager) regenerateCache() error {
	m.asgCache.mutex.Lock()
	defer m.asgCache.mutex.Unlock()
	return m.asgCache.regenerate()
}

// Cleanup the ASG cache.
func (m *OKEManager) Cleanup() {
	m.asgCache.Cleanup()
}

func (m *OKEManager) getFilteredAutoscalingGroups(filter []cloudprovider.LabelAutoDiscoveryConfig) (asgs []cloudprovider.NodeGroup, err error) {
	if len(filter) == 0 {
		return nil, nil
	}

	return nil, nil

}

// listScaleSets gets a list of scale sets and instanceIDs.
func (m *OKEManager) listScaleSets(filter []cloudprovider.LabelAutoDiscoveryConfig) (asgs []cloudprovider.NodeGroup, err error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	result, err := m.azClient.virtualMachineScaleSetsClient.List(ctx, m.config.ResourceGroup)
	if err != nil {
		glog.Errorf("VirtualMachineScaleSetsClient.List for %v failed: %v", m.config.ResourceGroup, err)
		return nil, err
	}

	for _, scaleSet := range result {
		if len(filter) > 0 {
			if scaleSet.Tags == nil || len(scaleSet.Tags) == 0 {
				continue
			}

			if !matchDiscoveryConfig(scaleSet.Tags, filter) {
				continue
			}
		}

		spec := &dynamic.NodeGroupSpec{
			Name:               *scaleSet.Name,
			MinSize:            1,
			MaxSize:            -1,
			SupportScaleToZero: scaleToZeroSupportedVMSS,
		}
		asg, _ := NewScaleSet(spec, m)
		asgs = append(asgs, asg)
	}

	return asgs, nil
}

// listAgentPools gets a list of agent pools and instanceIDs.
// Note: filter won't take effect for agent pools.
func (m *OKEManager) listAgentPools(filter []cloudprovider.LabelAutoDiscoveryConfig) (asgs []cloudprovider.NodeGroup, err error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()
	deploy, err := m.azClient.deploymentsClient.Get(ctx, m.config.ResourceGroup, m.config.Deployment)
	if err != nil {
		glog.Errorf("deploymentsClient.Get(%s, %s) failed: %v", m.config.ResourceGroup, m.config.Deployment, err)
		return nil, err
	}

	parameters := deploy.Properties.Parameters.(map[string]interface{})
	for k := range parameters {
		if k == "masterVMSize" || !strings.HasSuffix(k, "VMSize") {
			continue
		}

		poolName := strings.TrimRight(k, "VMSize")
		spec := &dynamic.NodeGroupSpec{
			Name:               poolName,
			MinSize:            1,
			MaxSize:            -1,
			SupportScaleToZero: scaleToZeroSupportedStandard,
		}
		asg, _ := NewAgentPool(spec, m)
		asgs = append(asgs, asg)
	}

	return asgs, nil
}

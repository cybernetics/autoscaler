/*
Copyright 2016 The Kubernetes Authors.

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

package gce

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	gcfg "gopkg.in/gcfg.v1"

	"cloud.google.com/go/compute/metadata"
	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gce "google.golang.org/api/compute/v1"
	gke "google.golang.org/api/container/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	provider_gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
)

// GcpCloudProviderMode allows to pass information whether the cluster is GCE or GKE.
type GcpCloudProviderMode string

const (
	// ModeGCE means that the cluster is running on gce (or using the legacy gke setup).
	ModeGCE GcpCloudProviderMode = "gce"

	// ModeGKE means that the cluster is running
	ModeGKE GcpCloudProviderMode = "gke"
)

const (
	operationWaitTimeout       = 5 * time.Second
	operationPollInterval      = 100 * time.Millisecond
	nodeAutoprovisioningPrefix = "nodeautoprovisioning"
)

type migInformation struct {
	config   *Mig
	basename string
}

// GceManager is handles gce communication and data caching.
type GceManager struct {
	migs     []*migInformation
	migCache map[GceRef]*Mig

	gceService *gce.Service
	gkeService *gke.Service

	cacheMutex sync.Mutex
	migsMutex  sync.Mutex

	zone        string
	projectId   string
	clusterName string
	mode        GcpCloudProviderMode
	templates   *templateBuilder
}

// CreateGceManager constructs gceManager object.
func CreateGceManager(configReader io.Reader, mode GcpCloudProviderMode, clusterName string) (*GceManager, error) {
	// Create Google Compute Engine token.
	tokenSource := google.ComputeTokenSource("")
	if configReader != nil {
		var cfg provider_gce.ConfigFile
		if err := gcfg.ReadInto(&cfg, configReader); err != nil {
			glog.Errorf("Couldn't read config: %v", err)
			return nil, err
		}
		if cfg.Global.TokenURL == "" {
			glog.Warning("Empty tokenUrl in cloud config")
		} else {
			glog.Infof("Using TokenSource from config %#v", tokenSource)
			tokenSource = provider_gce.NewAltTokenSource(cfg.Global.TokenURL, cfg.Global.TokenBody)
		}
	} else {
		glog.Infof("Using default TokenSource %#v", tokenSource)
	}
	projectId, zone, err := getProjectAndZone()
	if err != nil {
		return nil, err
	}
	glog.V(1).Infof("GCE projectId=%s zone=%s", projectId, zone)

	// Create Google Compute Engine service.
	client := oauth2.NewClient(oauth2.NoContext, tokenSource)
	gceService, err := gce.New(client)
	if err != nil {
		return nil, err
	}
	manager := &GceManager{
		migs:        make([]*migInformation, 0),
		gceService:  gceService,
		migCache:    make(map[GceRef]*Mig),
		zone:        zone,
		projectId:   projectId,
		clusterName: clusterName,
		mode:        mode,
		templates: &templateBuilder{
			projectId: projectId,
			zone:      zone,
			service:   gceService,
		},
	}

	if mode == ModeGKE {
		gkeService, err := gke.New(client)
		if err != nil {
			return nil, err
		}
		manager.gkeService = gkeService
		err = manager.fetchAllNodePools()
		if err != nil {
			glog.Errorf("Failed to fech node pools: %v", err)
		}
	}

	go wait.Forever(func() {
		manager.cacheMutex.Lock()
		defer manager.cacheMutex.Unlock()
		if err := manager.regenerateCache(); err != nil {
			glog.Errorf("Error while regenerating Mig cache: %v", err)
		}
	}, time.Hour)

	return manager, nil
}

func (m *GceManager) assertGKE() {
	if m.mode != ModeGKE {
		panic(fmt.Errorf("This should run only in GKE mode"))
	}
}

// Gets all registered node pools
func (m *GceManager) fetchAllNodePools() error {
	m.assertGKE()

	nodePoolsResponse, err := m.gkeService.Projects.Zones.Clusters.NodePools.List(m.projectId, m.zone, m.clusterName).Do()
	if err != nil {
		return err
	}
	for _, nodePool := range nodePoolsResponse.NodePools {
		autoprovisioned := strings.Contains("name", nodeAutoprovisioningPrefix)
		autoscaled := nodePool.Autoscaling != nil && nodePool.Autoscaling.Enabled
		if !autoprovisioned && !autoscaled {
			continue
		}
		// format is
		// "https://www.googleapis.com/compute/v1/projects/mwielgus-proj/zones/europe-west1-b/instanceGroupManagers/gke-cluster-1-default-pool-ba78a787-grp"
		for _, igurl := range nodePool.InstanceGroupUrls {
			project, zone, name, err := parseGceUrl(igurl, "instanceGroupManagers")
			if err != nil {
				return err
			}
			mig := &Mig{
				GceRef: GceRef{
					Name:    name,
					Zone:    zone,
					Project: project,
				},
				gceManager:      m,
				exist:           true,
				autoprovisioned: autoprovisioned,
			}
			if autoscaled {
				mig.minSize = int(nodePool.Autoscaling.MinNodeCount)
				mig.maxSize = int(nodePool.Autoscaling.MaxNodeCount)
			} else if autoprovisioned {
				mig.minSize = minAutoprovisionedSize
				mig.maxSize = maxAutoprovisionedSize
			}
			m.RegisterMig(mig)
		}
		// TODO - unregister migs
	}
	return nil
}

// RegisterMig registers mig in Gce Manager. Returns true if the node group didn't exist before.
func (m *GceManager) RegisterMig(mig *Mig) bool {
	m.migsMutex.Lock()
	defer m.migsMutex.Unlock()

	updated := false
	for i := range m.migs {
		if m.migs[i].config.GceRef == mig.GceRef {
			m.migs[i].config = mig
			glog.V(8).Infof("Updated Mig %s/%s/%s", mig.GceRef.Project, mig.GceRef.Zone, mig.GceRef.Name)
			updated = true
		}
	}

	if !updated {
		glog.V(1).Infof("Registering %s/%s/%s", mig.GceRef.Project, mig.GceRef.Zone, mig.GceRef.Name)
		m.migs = append(m.migs, &migInformation{
			config: mig,
		})
	}

	template, err := m.templates.getMigTemplate(mig)
	if err != nil {
		glog.Errorf("Failed to build template for %s", mig.Name)
	}

	_, err = m.templates.buildNodeFromTemplate(mig, template)
	if err != nil {
		glog.Errorf("Failed to build template for %s", mig.Name)
	}
	return !updated
}

// GetMigSize gets MIG size.
func (m *GceManager) GetMigSize(mig *Mig) (int64, error) {
	igm, err := m.gceService.InstanceGroupManagers.Get(mig.Project, mig.Zone, mig.Name).Do()
	if err != nil {
		return -1, err
	}
	return igm.TargetSize, nil
}

// SetMigSize sets MIG size.
func (m *GceManager) SetMigSize(mig *Mig, size int64) error {
	glog.V(0).Infof("Setting mig size %s to %d", mig.Id(), size)
	op, err := m.gceService.InstanceGroupManagers.Resize(mig.Project, mig.Zone, mig.Name, size).Do()
	if err != nil {
		return err
	}
	if err := m.waitForOp(op, mig.Project, mig.Zone); err != nil {
		return err
	}
	return nil
}

func (m *GceManager) waitForOp(operation *gce.Operation, project string, zone string) error {
	for start := time.Now(); time.Since(start) < operationWaitTimeout; time.Sleep(operationPollInterval) {
		glog.V(4).Infof("Waiting for operation %s %s %s", project, zone, operation.Name)
		if op, err := m.gceService.ZoneOperations.Get(project, zone, operation.Name).Do(); err == nil {
			glog.V(4).Infof("Operation %s %s %s status: %s", project, zone, operation.Name, op.Status)
			if op.Status == "DONE" {
				return nil
			}
		} else {
			glog.Warningf("Error while getting operation %s on %s: %v", operation.Name, operation.TargetLink, err)
		}
	}
	return fmt.Errorf("Timeout while waiting for operation %s on %s to complete.", operation.Name, operation.TargetLink)
}

// DeleteInstances deletes the given instances. All instances must be controlled by the same MIG.
func (m *GceManager) DeleteInstances(instances []*GceRef) error {
	if len(instances) == 0 {
		return nil
	}
	commonMig, err := m.GetMigForInstance(instances[0])
	if err != nil {
		return err
	}
	for _, instance := range instances {
		mig, err := m.GetMigForInstance(instance)
		if err != nil {
			return err
		}
		if mig != commonMig {
			return fmt.Errorf("Connot delete instances which don't belong to the same MIG.")
		}
	}

	req := gce.InstanceGroupManagersDeleteInstancesRequest{
		Instances: []string{},
	}
	for _, instance := range instances {
		req.Instances = append(req.Instances, GenerateInstanceUrl(instance.Project, instance.Zone, instance.Name))
	}

	op, err := m.gceService.InstanceGroupManagers.DeleteInstances(commonMig.Project, commonMig.Zone, commonMig.Name, &req).Do()
	if err != nil {
		return err
	}
	if err := m.waitForOp(op, commonMig.Project, commonMig.Zone); err != nil {
		return err
	}
	return nil
}

func (m *GceManager) getMigs() []*migInformation {
	m.migsMutex.Lock()
	defer m.migsMutex.Unlock()
	migs := make([]*migInformation, 0, len(m.migs))
	for _, mig := range m.migs {
		migs = append(migs, &migInformation{
			basename: mig.basename,
			config:   mig.config,
		})
	}
	return migs
}

// GetMigForInstance returns MigConfig of the given Instance
func (m *GceManager) GetMigForInstance(instance *GceRef) (*Mig, error) {
	m.cacheMutex.Lock()
	defer m.cacheMutex.Unlock()
	if mig, found := m.migCache[*instance]; found {
		return mig, nil
	}

	for _, mig := range m.getMigs() {
		if mig.config.Project == instance.Project &&
			mig.config.Zone == instance.Zone &&
			strings.HasPrefix(instance.Name, mig.basename) {
			if err := m.regenerateCache(); err != nil {
				return nil, fmt.Errorf("Error while looking for MIG for instance %+v, error: %v", *instance, err)
			}
			if mig, found := m.migCache[*instance]; found {
				return mig, nil
			}
			return nil, fmt.Errorf("Instance %+v does not belong to any configured MIG", *instance)
		}
	}
	// Instance doesn't belong to any configured mig.
	return nil, nil
}

func (m *GceManager) regenerateCache() error {
	newMigCache := make(map[GceRef]*Mig)

	for _, migInfo := range m.getMigs() {
		mig := migInfo.config
		glog.V(4).Infof("Regenerating MIG information for %s %s %s", mig.Project, mig.Zone, mig.Name)

		instanceGroupManager, err := m.gceService.InstanceGroupManagers.Get(mig.Project, mig.Zone, mig.Name).Do()
		if err != nil {
			return err
		}
		migInfo.basename = instanceGroupManager.BaseInstanceName

		instances, err := m.gceService.InstanceGroupManagers.ListManagedInstances(mig.Project, mig.Zone, mig.Name).Do()
		if err != nil {
			glog.V(4).Infof("Failed MIG info request for %s %s %s: %v", mig.Project, mig.Zone, mig.Name, err)
			return err
		}
		for _, instance := range instances.ManagedInstances {
			project, zone, name, err := ParseInstanceUrl(instance.Instance)
			if err != nil {
				return err
			}
			newMigCache[GceRef{Project: project, Zone: zone, Name: name}] = mig
		}
	}

	m.migCache = newMigCache
	return nil
}

// GetMigNodes returns mig nodes.
func (m *GceManager) GetMigNodes(mig *Mig) ([]string, error) {
	instances, err := m.gceService.InstanceGroupManagers.ListManagedInstances(mig.Project, mig.Zone, mig.Name).Do()
	if err != nil {
		return []string{}, err
	}
	result := make([]string, 0)
	for _, instance := range instances.ManagedInstances {
		project, zone, name, err := ParseInstanceUrl(instance.Instance)
		if err != nil {
			return []string{}, err
		}
		result = append(result, fmt.Sprintf("gce://%s/%s/%s", project, zone, name))
	}
	return result, nil
}

// Code borrowed from gce cloud provider. Reuse the original as soon as it becomes public.
func getProjectAndZone() (string, string, error) {
	result, err := metadata.Get("instance/zone")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(result, "/")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("unexpected response: %s", result)
	}
	zone := parts[3]
	projectID, err := metadata.ProjectID()
	if err != nil {
		return "", "", err
	}
	return projectID, zone, nil
}

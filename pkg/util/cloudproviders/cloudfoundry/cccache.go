// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build clusterchecks
// +build clusterchecks

package cloudfoundry

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudfoundry-community/go-cfclient"

	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// CCCacheI is an interface for a structure that caches and automatically refreshes data from Cloud Foundry API
// it's useful mostly to be able to mock CCCache during unit tests
type CCCacheI interface {
	// LastUpdated return the last time the cache was updated
	LastUpdated() time.Time

	// UpdatedOnce returns a channel that is closed once the cache has been updated
	// successfully at least once.  Successive calls to UpdatedOnce return the
	// same channel.  If the cache's context ends before an update occurs, this channel
	// will never close.
	UpdatedOnce() <-chan struct{}

	// GetApp looks for an app with the given GUID in the cache
	GetApp(string) (*cfclient.V3App, error)

	// GetSpace looks for a space with the given GUID in the cache
	GetSpace(string) (*cfclient.V3Space, error)

	// GetOrg looks for an org with the given GUID in the cache
	GetOrg(string) (*cfclient.V3Organization, error)

	// GetOrgs returns all orgs in the cache
	GetOrgs() ([]*cfclient.V3Organization, error)

	// GetOrgQuotas returns all orgs quotas in the cache
	GetOrgQuotas() ([]*CFOrgQuota, error)

	// GetCFApplication looks for a CF application with the given GUID in the cache
	GetCFApplication(string) (*CFApplication, error)

	// GetCFApplications returns all CF applications in the cache
	GetCFApplications() ([]*CFApplication, error)

	// GetSidecars returns all sidecars for the given application GUID in the cache
	GetSidecars(string) ([]*CFSidecar, error)

	// GetIsolationSegmentForSpace returns an isolation segment for the given GUID in the cache
	GetIsolationSegmentForSpace(string) (*cfclient.IsolationSegment, error)

	// GetIsolationSegmentForOrg returns an isolation segment for the given GUID in the cache
	GetIsolationSegmentForOrg(string) (*cfclient.IsolationSegment, error)
}

// CCCache is a simple structure that caches and automatically refreshes data from Cloud Foundry API
type CCCache struct {
	sync.RWMutex
	cancelContext        context.Context
	configured           bool
	refreshCacheOnMiss   bool
	serveNozzleData      bool
	sidecarsTags         bool
	segmentsTags         bool
	ccAPIClient          CCClientI
	pollInterval         time.Duration
	lastUpdated          time.Time
	updatedOnce          chan struct{}
	appsByGUID           map[string]*cfclient.V3App
	orgsByGUID           map[string]*cfclient.V3Organization
	orgQuotasByGUID      map[string]*CFOrgQuota
	spacesByGUID         map[string]*cfclient.V3Space
	processesByAppGUID   map[string][]*cfclient.Process
	cfApplicationsByGUID map[string]*CFApplication
	sidecarsByAppGUID    map[string][]*CFSidecar
	segmentBySpaceGUID   map[string]*cfclient.IsolationSegment
	segmentByOrgGUID     map[string]*cfclient.IsolationSegment
	appsBatchSize        int
}

// CCClientI is an interface for a Cloud Foundry Client that queries the Cloud Foundry API
type CCClientI interface {
	ListV3AppsByQuery(url.Values) ([]cfclient.V3App, error)
	ListV3OrganizationsByQuery(url.Values) ([]cfclient.V3Organization, error)
	ListV3SpacesByQuery(url.Values) ([]cfclient.V3Space, error)
	ListAllProcessesByQuery(url.Values) ([]cfclient.Process, error)
	ListOrgQuotasByQuery(url.Values) ([]cfclient.OrgQuota, error)
	ListSidecarsByApp(url.Values, string) ([]CFSidecar, error)
	ListIsolationSegmentsByQuery(url.Values) ([]cfclient.IsolationSegment, error)
	GetIsolationSegmentSpaceGUID(string) (string, error)
	GetIsolationSegmentOrganizationGUID(string) (string, error)
}

var globalCCCache = &CCCache{}

// ConfigureGlobalCCCache configures the global instance of CCCache from provided config
func ConfigureGlobalCCCache(ctx context.Context, ccURL, ccClientID, ccClientSecret string, skipSSLValidation bool, pollInterval time.Duration, appsBatchSize int, refreshCacheOnMiss, serveNozzleData, sidecarsTags, segmentsTags bool, testing CCClientI) (*CCCache, error) {
	globalCCCache.Lock()
	defer globalCCCache.Unlock()

	if globalCCCache.configured {
		return globalCCCache, nil
	}

	if testing != nil {
		globalCCCache.ccAPIClient = testing
	} else {
		clientConfig := &cfclient.Config{
			ApiAddress:        ccURL,
			ClientID:          ccClientID,
			ClientSecret:      ccClientSecret,
			SkipSslValidation: skipSSLValidation,
			UserAgent:         "datadog-cluster-agent",
		}
		var err error
		globalCCCache.ccAPIClient, err = NewCFClient(clientConfig)
		if err != nil {
			return nil, err
		}
	}

	globalCCCache.pollInterval = pollInterval
	globalCCCache.appsBatchSize = appsBatchSize
	globalCCCache.lastUpdated = time.Time{} // zero time
	globalCCCache.updatedOnce = make(chan struct{})
	globalCCCache.cancelContext = ctx
	globalCCCache.configured = true
	globalCCCache.refreshCacheOnMiss = refreshCacheOnMiss
	globalCCCache.serveNozzleData = serveNozzleData
	globalCCCache.sidecarsTags = sidecarsTags
	globalCCCache.segmentsTags = segmentsTags

	go globalCCCache.start()

	return globalCCCache, nil
}

// GetGlobalCCCache returns the global instance of CCCache (or error if the instance is not configured yet)
func GetGlobalCCCache() (*CCCache, error) {
	globalCCCache.Lock()
	defer globalCCCache.Unlock()
	if !globalCCCache.configured {
		return nil, fmt.Errorf("global CC Cache not configured")
	}
	return globalCCCache, nil
}

// LastUpdated return the last time the cache was updated
func (ccc *CCCache) LastUpdated() time.Time {
	ccc.RLock()
	defer ccc.RUnlock()
	return ccc.lastUpdated
}

// UpdatedOnce returns a channel that is closed once the cache has been updated
// successfully at least once.  Successive calls to UpdatedOnce return the
// same channel.  If the cache's context ends before an update occurs, this channel
// will never close.
func (ccc *CCCache) UpdatedOnce() <-chan struct{} {
	return ccc.updatedOnce
}

// GetOrgs returns all orgs in the cache
func (ccc *CCCache) GetOrgs() ([]*cfclient.V3Organization, error) {
	ccc.RLock()
	defer ccc.RUnlock()

	var orgs []*cfclient.V3Organization
	for _, org := range ccc.orgsByGUID {
		orgs = append(orgs, org)
	}

	return orgs, nil
}

// GetOrgQuotas returns all org quotas in the cache
func (ccc *CCCache) GetOrgQuotas() ([]*CFOrgQuota, error) {
	ccc.RLock()
	defer ccc.RUnlock()

	var orgQuotas []*CFOrgQuota
	for _, org := range ccc.orgQuotasByGUID {
		orgQuotas = append(orgQuotas, org)
	}

	return orgQuotas, nil
}

// GetCFApplications returns all CF applications in the cache
func (ccc *CCCache) GetCFApplications() ([]*CFApplication, error) {
	ccc.RLock()
	defer ccc.RUnlock()

	var cfapps []*CFApplication
	for _, cfapp := range ccc.cfApplicationsByGUID {
		cfapps = append(cfapps, cfapp)
	}

	return cfapps, nil
}

// GetCFApplication looks for a CF application with the given GUID in the cache
func (ccc *CCCache) GetCFApplication(guid string) (*CFApplication, error) {
	var cfapp *CFApplication
	var ok bool

	ccc.RLock()
	cfapp, ok = ccc.cfApplicationsByGUID[guid]
	ccc.RUnlock()
	if !ok {
		if !ccc.refreshCacheOnMiss {
			return nil, fmt.Errorf("could not find CF application %s in cloud controller cache", guid)
		}
		ccc.readData()
		ccc.RLock()
		cfapp, ok = ccc.cfApplicationsByGUID[guid]
		ccc.RUnlock()
		if !ok {
			return nil, fmt.Errorf("could not find CF application %s in cloud controller cache", guid)
		}
	}
	return cfapp, nil
}

// GetSidecars looks for sidecars of an app with the given GUID in the cache
func (ccc *CCCache) GetSidecars(guid string) ([]*CFSidecar, error) {
	ccc.RLock()
	defer ccc.RUnlock()

	sidecars, ok := ccc.sidecarsByAppGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find sidecars for app %s in cloud controller cache", guid)
	}
	return sidecars, nil
}

// GetApp looks for an app with the given GUID in the cache
func (ccc *CCCache) GetApp(guid string) (*cfclient.V3App, error) {
	ccc.RLock()
	defer ccc.RUnlock()

	app, ok := ccc.appsByGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find app %s in cloud controller cache", guid)
	}
	return app, nil
}

// GetSpace looks for a space with the given GUID in the cache
func (ccc *CCCache) GetSpace(guid string) (*cfclient.V3Space, error) {
	ccc.RLock()
	defer ccc.RUnlock()
	space, ok := ccc.spacesByGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find space %s in cloud controller cache", guid)
	}
	return space, nil
}

// GetOrg looks for an org with the given GUID in the cache
func (ccc *CCCache) GetOrg(guid string) (*cfclient.V3Organization, error) {
	ccc.RLock()
	defer ccc.RUnlock()
	org, ok := ccc.orgsByGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find org %s in cloud controller cache", guid)
	}
	return org, nil
}

// GetIsolationSegmentForSpace returns an isolation segment for the given GUID in the cache
func (ccc *CCCache) GetIsolationSegmentForSpace(guid string) (*cfclient.IsolationSegment, error) {
	ccc.RLock()
	defer ccc.RUnlock()
	segment, ok := ccc.segmentBySpaceGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find isolation segment for space %s in cloud controller cache", guid)
	}
	return segment, nil
}

// GetIsolationSegmentForOrg returns an isolation segment for the given GUID in the cache
func (ccc *CCCache) GetIsolationSegmentForOrg(guid string) (*cfclient.IsolationSegment, error) {
	ccc.RLock()
	defer ccc.RUnlock()
	segment, ok := ccc.segmentByOrgGUID[guid]
	if !ok {
		return nil, fmt.Errorf("could not find isolation segment for organization %s in cloud controller cache", guid)
	}
	return segment, nil
}

func (ccc *CCCache) start() {
	ccc.readData()
	dataRefreshTicker := time.NewTicker(ccc.pollInterval)
	for {
		select {
		case <-dataRefreshTicker.C:
			ccc.readData()
		case <-ccc.cancelContext.Done():
			dataRefreshTicker.Stop()
			return
		}
	}
}

func (ccc *CCCache) readData() {
	log.Debug("Reading data from CC API")
	var wg sync.WaitGroup
	var err error

	// List applications
	wg.Add(1)
	var appsByGUID map[string]*cfclient.V3App
	var apps []cfclient.V3App

	var sidecarsByAppGUID map[string][]*CFSidecar

	go func() {
		defer wg.Done()
		query := url.Values{}
		query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
		apps, err = ccc.ccAPIClient.ListV3AppsByQuery(query)
		if err != nil {
			log.Errorf("Failed listing apps from cloud controller: %v", err)
			return
		}
		appsByGUID = make(map[string]*cfclient.V3App, len(apps))
		sidecarsByAppGUID = make(map[string][]*CFSidecar)
		for _, app := range apps {
			v3App := app
			appsByGUID[app.GUID] = &v3App

			if ccc.sidecarsTags {
				// list app sidecars
				var allSidecars []*CFSidecar
				sidecars, err := ccc.ccAPIClient.ListSidecarsByApp(query, app.GUID)
				if err != nil {
					log.Errorf("Failed listing sidecars from cloud controller: %v", err)
					return
				}
				for _, sidecar := range sidecars {
					s := sidecar
					allSidecars = append(allSidecars, &s)
				}
				sidecarsByAppGUID[app.GUID] = allSidecars
			}
		}
	}()

	// List spaces
	wg.Add(1)
	var spacesByGUID map[string]*cfclient.V3Space
	go func() {
		defer wg.Done()
		query := url.Values{}
		query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
		spaces, err := ccc.ccAPIClient.ListV3SpacesByQuery(query)
		if err != nil {
			log.Errorf("Failed listing spaces from cloud controller: %v", err)
			return
		}
		spacesByGUID = make(map[string]*cfclient.V3Space, len(spaces))
		for _, space := range spaces {
			v3Space := space
			spacesByGUID[space.GUID] = &v3Space
		}

	}()

	// List orgs
	wg.Add(1)
	var orgsByGUID map[string]*cfclient.V3Organization
	go func() {
		defer wg.Done()
		query := url.Values{}
		query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
		orgs, err := ccc.ccAPIClient.ListV3OrganizationsByQuery(query)
		if err != nil {
			log.Errorf("Failed listing orgs from cloud controller: %v", err)
			return
		}
		orgsByGUID = make(map[string]*cfclient.V3Organization, len(orgs))
		for _, org := range orgs {
			v3Org := org
			orgsByGUID[org.GUID] = &v3Org
		}
	}()

	// List orgQuotas
	wg.Add(1)
	var orgQuotasByGUID map[string]*CFOrgQuota
	go func() {
		defer wg.Done()
		query := url.Values{}
		query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
		orgQuotas, err := ccc.ccAPIClient.ListOrgQuotasByQuery(query)
		if err != nil {
			log.Errorf("Failed listing org quotas from cloud controller: %v", err)
			return
		}
		orgQuotasByGUID = make(map[string]*CFOrgQuota, len(orgQuotas))
		for _, orgQuota := range orgQuotas {
			q := CFOrgQuota{
				GUID:        orgQuota.Guid,
				MemoryLimit: orgQuota.MemoryLimit,
			}
			orgQuotasByGUID[orgQuota.Guid] = &q
		}
	}()

	// List processes
	wg.Add(1)
	var processesByAppGUID map[string][]*cfclient.Process
	go func() {
		defer wg.Done()
		query := url.Values{}
		query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
		processes, err := ccc.ccAPIClient.ListAllProcessesByQuery(query)
		if err != nil {
			log.Errorf("Failed listing processes from cloud controller: %v", err)
			return
		}
		// Group all processes per app
		processesByAppGUID = make(map[string][]*cfclient.Process)
		for _, process := range processes {
			v3Process := process
			parts := strings.Split(v3Process.Links.App.Href, "/")
			appGUID := parts[len(parts)-1]
			appProcesses, exists := processesByAppGUID[appGUID]
			if exists {
				appProcesses = append(appProcesses, &v3Process)
			} else {
				appProcesses = []*cfclient.Process{&v3Process}
			}
			processesByAppGUID[appGUID] = appProcesses
		}
	}()

	var segmentBySpaceGUID map[string]*cfclient.IsolationSegment
	var segmentByOrgGUID map[string]*cfclient.IsolationSegment

	if ccc.segmentsTags {
		// List isolation segments
		wg.Add(1)

		go func() {
			defer wg.Done()
			query := url.Values{}
			query.Add("per_page", fmt.Sprintf("%d", ccc.appsBatchSize))
			segments, err := ccc.ccAPIClient.ListIsolationSegmentsByQuery(query)
			if err != nil {
				log.Errorf("Failed listing isolation segments from cloud controller: %v", err)
				return
			}
			segmentBySpaceGUID = make(map[string]*cfclient.IsolationSegment)
			segmentByOrgGUID = make(map[string]*cfclient.IsolationSegment)
			for _, segment := range segments {
				s := segment
				spaceGUID, err := ccc.ccAPIClient.GetIsolationSegmentSpaceGUID(segment.GUID)
				if err == nil {
					if spaceGUID != "" {
						segmentBySpaceGUID[spaceGUID] = &s
					}
				} else {
					log.Errorf("Failed listing isolation segment space for segment %s: %v", segment.Name, err)
				}

				orgGUID, err := ccc.ccAPIClient.GetIsolationSegmentOrganizationGUID(segment.GUID)
				if err == nil {
					if orgGUID != "" {
						segmentByOrgGUID[orgGUID] = &s
					}
				} else {
					log.Errorf("Failed listing isolation segment organization for segment %s: %v", segment.Name, err)
				}

			}
		}()
	}

	// wait for resources acquisition
	wg.Wait()

	// prepare CFApplications
	var cfApplicationsByGUID map[string]*CFApplication
	if ccc.serveNozzleData {
		cfApplicationsByGUID = make(map[string]*CFApplication, len(apps))
		// Populate cfApplications
		for _, cfapp := range apps {
			updatedApp := CFApplication{}
			updatedApp.extractDataFromV3App(cfapp)
			appGUID := updatedApp.GUID
			spaceGUID := updatedApp.SpaceGUID
			processes, exists := processesByAppGUID[appGUID]
			if exists {
				updatedApp.extractDataFromV3Process(processes)
			} else {
				log.Infof("could not fetch processes info for app guid %s", appGUID)
			}
			// Fill space then org data. Order matters for labels and annotations.
			space, exists := spacesByGUID[spaceGUID]
			if exists {
				updatedApp.extractDataFromV3Space(space)
			} else {
				log.Infof("could not fetch space info for space guid %s", spaceGUID)
			}
			orgGUID := updatedApp.OrgGUID
			org, exists := orgsByGUID[orgGUID]
			if exists {
				updatedApp.extractDataFromV3Org(org)
			} else {
				log.Infof("could not fetch org info for org guid %s", orgGUID)
			}
			for _, sidecar := range sidecarsByAppGUID[appGUID] {
				updatedApp.Sidecars = append(updatedApp.Sidecars, *sidecar)
			}
			cfApplicationsByGUID[appGUID] = &updatedApp
		}
	}

	// put new data in cache
	ccc.Lock()
	defer ccc.Unlock()

	ccc.segmentBySpaceGUID = segmentBySpaceGUID
	ccc.segmentByOrgGUID = segmentByOrgGUID
	ccc.sidecarsByAppGUID = sidecarsByAppGUID
	ccc.appsByGUID = appsByGUID
	ccc.spacesByGUID = spacesByGUID
	ccc.orgsByGUID = orgsByGUID
	ccc.orgQuotasByGUID = orgQuotasByGUID
	ccc.processesByAppGUID = processesByAppGUID
	ccc.cfApplicationsByGUID = cfApplicationsByGUID
	firstUpdate := ccc.lastUpdated.IsZero()
	ccc.lastUpdated = time.Now()
	if firstUpdate {
		close(ccc.updatedOnce)
	}
}

// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"

	model "github.com/DataDog/agent-payload/v5/process"
	ddconfig "github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/config/resolver"
	"github.com/DataDog/datadog-agent/pkg/forwarder"
	"github.com/DataDog/datadog-agent/pkg/orchestrator"
	"github.com/DataDog/datadog-agent/pkg/process/checks"
	"github.com/DataDog/datadog-agent/pkg/process/config"
	"github.com/DataDog/datadog-agent/pkg/process/statsd"
	"github.com/DataDog/datadog-agent/pkg/process/util"
	"github.com/DataDog/datadog-agent/pkg/process/util/api"
	apicfg "github.com/DataDog/datadog-agent/pkg/process/util/api/config"
	"github.com/DataDog/datadog-agent/pkg/process/util/api/headers"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/clustername"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/datadog-agent/pkg/version"

	"go.uber.org/atomic"
)

type checkResult struct {
	name        string
	payloads    []checkPayload
	sizeInBytes int64
}

func (cr *checkResult) Weight() int64 {
	return cr.sizeInBytes
}

func (cr *checkResult) Type() string {
	return cr.name
}

var _ api.WeightedItem = &checkResult{}

type checkPayload struct {
	body    []byte
	headers http.Header
}

// Collector will collect metrics from the local system and ship to the backend.
type Collector struct {
	// true if real-time is enabled
	realTimeEnabled *atomic.Bool

	// the next groupID to be issued
	groupID *atomic.Int32

	rtIntervalCh chan time.Duration
	cfg          *config.AgentConfig

	// counters for each type of check
	runCounters   sync.Map
	enabledChecks []checks.Check

	// Controls the real-time interval, can change live.
	realTimeInterval time.Duration

	processResults   *api.WeightedQueue
	rtProcessResults *api.WeightedQueue
	eventResults     *api.WeightedQueue

	connectionsResults *api.WeightedQueue

	podResults *api.WeightedQueue

	forwarderRetryQueueMaxBytes int

	// Enables running realtime checks
	runRealTime bool

	// Drop payloads from specified checks
	dropCheckPayloads []string
}

// NewCollector creates a new Collector
func NewCollector(cfg *config.AgentConfig, enabledChecks []checks.Check) (Collector, error) {
	sysInfo, err := checks.CollectSystemInfo(cfg)
	if err != nil {
		return Collector{}, err
	}

	runRealTime := !ddconfig.Datadog.GetBool("process_config.disable_realtime_checks")
	for _, c := range enabledChecks {
		c.Init(cfg, sysInfo)
	}

	return NewCollectorWithChecks(cfg, enabledChecks, runRealTime), nil
}

// NewCollectorWithChecks creates a new Collector
func NewCollectorWithChecks(cfg *config.AgentConfig, checks []checks.Check, runRealTime bool) Collector {
	queueSize := ddconfig.Datadog.GetInt("process_config.queue_size")
	if queueSize <= 0 {
		log.Warnf("Invalid check queue size: %d. Using default value: %d", queueSize, ddconfig.DefaultProcessQueueSize)
		queueSize = ddconfig.DefaultProcessQueueSize
	}

	rtQueueSize := ddconfig.Datadog.GetInt("process_config.rt_queue_size")
	if rtQueueSize <= 0 {
		log.Warnf("Invalid rt check queue size: %d. Using default value: %d", rtQueueSize, ddconfig.DefaultProcessRTQueueSize)
		rtQueueSize = ddconfig.DefaultProcessRTQueueSize
	}

	queueBytes := ddconfig.Datadog.GetInt("process_config.process_queue_bytes")
	if queueBytes <= 0 {
		log.Warnf("Invalid queue bytes size: %d. Using default value: %d", queueBytes, ddconfig.DefaultProcessQueueBytes)
		queueBytes = ddconfig.DefaultProcessQueueBytes
	}

	processResults := api.NewWeightedQueue(queueSize, int64(queueBytes))
	log.Debugf("Creating process check queue with max_size=%d and max_weight=%d", processResults.MaxSize(), processResults.MaxWeight())

	// reuse main queue's ProcessQueueBytes because it's unlikely that it'll reach to that size in bytes, so we don't need a separate config for it
	rtProcessResults := api.NewWeightedQueue(rtQueueSize, int64(queueBytes))
	log.Debugf("Creating rt process check queue with max_size=%d and max_weight=%d", rtProcessResults.MaxSize(), rtProcessResults.MaxWeight())

	connectionsResults := api.NewWeightedQueue(queueSize, int64(queueBytes))
	log.Debugf("Creating connections queue with max_size=%d and max_weight=%d", connectionsResults.MaxSize(), connectionsResults.MaxWeight())

	podResults := api.NewWeightedQueue(queueSize, int64(cfg.Orchestrator.PodQueueBytes))
	log.Debugf("Creating pod check queue with max_size=%d and max_weight=%d", podResults.MaxSize(), podResults.MaxWeight())

	eventResults := api.NewWeightedQueue(queueSize, int64(queueBytes))
	log.Debugf("Creating event check queue with max_size=%d and max_weight=%d", eventResults.MaxSize(), eventResults.MaxWeight())

	dropCheckPayloads := ddconfig.Datadog.GetStringSlice("process_config.drop_check_payloads")
	if len(dropCheckPayloads) > 0 {
		log.Debugf("Dropping payloads from checks: %v", dropCheckPayloads)
	}

	return Collector{
		rtIntervalCh:  make(chan time.Duration),
		cfg:           cfg,
		groupID:       atomic.NewInt32(rand.Int31()),
		enabledChecks: checks,

		// Defaults for real-time on start
		realTimeInterval: 2 * time.Second,
		realTimeEnabled:  atomic.NewBool(false),

		processResults:     processResults,
		rtProcessResults:   rtProcessResults,
		connectionsResults: connectionsResults,
		podResults:         podResults,
		eventResults:       eventResults,

		forwarderRetryQueueMaxBytes: queueBytes,

		runRealTime: runRealTime,

		dropCheckPayloads: dropCheckPayloads,
	}
}

func (l *Collector) runCheck(c checks.Check, results *api.WeightedQueue) {
	runCounter := l.nextRunCounter(c.Name())
	start := time.Now()
	// update the last collected timestamp for info
	updateLastCollectTime(start)

	messages, err := c.Run(l.cfg, l.nextGroupID())
	if err != nil {
		log.Errorf("Unable to run check '%s': %s", c.Name(), err)
		return
	}
	if c.ShouldSaveLastRun() {
		checks.StoreCheckOutput(c.Name(), messages)
	} else {
		checks.StoreCheckOutput(c.Name(), nil)
	}

	if c.Name() == config.PodCheckName {
		handlePodChecks(l, start, c.Name(), messages, results)
	} else {
		l.messagesToResults(start, c.Name(), messages, results)
	}

	if !c.RealTime() {
		logCheckDuration(c.Name(), start, runCounter)
	}
}

func (l *Collector) runCheckWithRealTime(c checks.CheckWithRealTime, results, rtResults *api.WeightedQueue, options checks.RunOptions) {
	start := time.Now()
	// update the last collected timestamp for info
	updateLastCollectTime(start)

	run, err := c.RunWithOptions(l.cfg, l.nextGroupID, options)
	if err != nil {
		log.Errorf("Unable to run check '%s': %s", c.Name(), err)
		return
	}
	l.messagesToResults(start, c.Name(), run.Standard, results)
	if options.RunStandard {
		// We are only updating the run counter for the standard check
		// since RT checks are too frequent and we only log standard check
		// durations
		runCounter := l.nextRunCounter(c.Name())
		checks.StoreCheckOutput(c.Name(), run.Standard)
		logCheckDuration(c.Name(), start, runCounter)
	}

	l.messagesToResults(start, c.RealTimeName(), run.RealTime, rtResults)
	if options.RunRealTime {
		checks.StoreCheckOutput(c.RealTimeName(), run.RealTime)
	}
}

func (l *Collector) nextRunCounter(name string) int32 {
	runCounter := int32(1)
	if rc, ok := l.runCounters.Load(name); ok {
		runCounter = rc.(int32) + 1
	}
	l.runCounters.Store(name, runCounter)
	return runCounter
}

func logCheckDuration(name string, start time.Time, runCounter int32) {
	d := time.Since(start)
	switch {
	case runCounter < 5:
		log.Infof("Finished %s check #%d in %s", name, runCounter, d)
	case runCounter == 5:
		log.Infof("Finished %s check #%d in %s. First 5 check runs finished, next runs will be logged every 20 runs.", name, runCounter, d)
	case runCounter%20 == 0:
		log.Infof("Finish %s check #%d in %s", name, runCounter, d)
	}
}

func (l *Collector) nextGroupID() int32 {
	return l.groupID.Inc()
}

func (l *Collector) messagesToResults(start time.Time, name string, messages []model.MessageBody, results *api.WeightedQueue) {
	if len(messages) == 0 {
		return
	}

	payloads := make([]checkPayload, 0, len(messages))
	sizeInBytes := 0

	for _, m := range messages {
		body, err := api.EncodePayload(m)
		if err != nil {
			log.Errorf("Unable to encode message: %s", err)
			continue
		}

		agentVersion, _ := version.Agent()
		extraHeaders := make(http.Header)
		extraHeaders.Set(headers.TimestampHeader, strconv.Itoa(int(start.Unix())))
		extraHeaders.Set(headers.HostHeader, l.cfg.HostName)
		extraHeaders.Set(headers.ProcessVersionHeader, agentVersion.GetNumber())
		extraHeaders.Set(headers.ContainerCountHeader, strconv.Itoa(getContainerCount(m)))
		extraHeaders.Set(headers.ContentTypeHeader, headers.ProtobufContentType)

		if l.cfg.Orchestrator.OrchestrationCollectionEnabled {
			if cid, err := clustername.GetClusterID(); err == nil && cid != "" {
				extraHeaders.Set(headers.ClusterIDHeader, cid)
			}
			extraHeaders.Set(headers.EVPOriginHeader, "process-agent")
			extraHeaders.Set(headers.EVPOriginVersionHeader, version.AgentVersion)

			switch m.(type) {
			case *model.CollectorManifest:
				extraHeaders.Set(headers.ContentEncodingHeader, headers.ZSTDContentEncoding)
			}
		}

		if name == checks.ProcessEvents.Name() {
			extraHeaders.Set(headers.EVPOriginHeader, "process-agent")
			extraHeaders.Set(headers.EVPOriginVersionHeader, version.AgentVersion)
		}

		payloads = append(payloads, checkPayload{
			body:    body,
			headers: extraHeaders,
		})

		sizeInBytes += len(body)
	}

	result := &checkResult{
		name:        name,
		payloads:    payloads,
		sizeInBytes: int64(sizeInBytes),
	}
	results.Add(result)
	// update proc and container count for info
	updateProcContainerCount(messages)
}

func (l *Collector) run(exit chan struct{}) error {
	processAPIEndpoints, err := getAPIEndpoints()
	if err != nil {
		return err
	}

	processEventsAPIEndpoints, err := getEventsAPIEndpoints()
	if err != nil {
		return err
	}

	eps := make([]string, 0, len(processAPIEndpoints))
	for _, e := range processAPIEndpoints {
		eps = append(eps, e.Endpoint.String())
	}
	orchestratorEps := make([]string, 0, len(l.cfg.Orchestrator.OrchestratorEndpoints))
	for _, e := range l.cfg.Orchestrator.OrchestratorEndpoints {
		orchestratorEps = append(orchestratorEps, e.Endpoint.String())
	}
	eventsEps := make([]string, 0, len(processEventsAPIEndpoints))
	for _, e := range processEventsAPIEndpoints {
		eventsEps = append(eventsEps, e.Endpoint.String())
	}

	var checkNames []string
	for _, check := range l.enabledChecks {
		checkNames = append(checkNames, check.Name())

		// Append `process_rt` if process check is enabled, and rt is enabled, so the customer doesn't get confused if
		// process_rt doesn't show up in the enabled checks
		if check.Name() == checks.Process.Name() && !ddconfig.Datadog.GetBool("process_config.disable_realtime_checks") {
			checkNames = append(checkNames, checks.Process.RealTimeName())
		}
	}
	updateEnabledChecks(checkNames)
	updateDropCheckPayloads(l.dropCheckPayloads)
	log.Infof("Starting process-agent for host=%s, endpoints=%s, events endpoints=%s orchestrator endpoints=%s, enabled checks=%v", l.cfg.HostName, eps, eventsEps, orchestratorEps, checkNames)

	go util.HandleSignals(exit)

	go func() {
		<-exit
		l.processResults.Stop()
		l.rtProcessResults.Stop()
		l.connectionsResults.Stop()
		l.podResults.Stop()
		l.eventResults.Stop()
	}()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		heartbeat := time.NewTicker(15 * time.Second)
		defer heartbeat.Stop()

		queueSizeTicker := time.NewTicker(10 * time.Second)
		defer queueSizeTicker.Stop()

		queueLogTicker := time.NewTicker(time.Minute)
		defer queueLogTicker.Stop()

		agentVersion, _ := version.Agent()
		tags := []string{
			fmt.Sprintf("version:%s", agentVersion.GetNumberAndPre()),
			fmt.Sprintf("revision:%s", agentVersion.Commit),
		}
		for {
			select {
			case <-heartbeat.C:
				statsd.Client.Gauge("datadog.process.agent", 1, tags, 1) //nolint:errcheck
			case <-queueSizeTicker.C:
				updateQueueStats(&queueStats{
					processQueueSize:      l.processResults.Len(),
					rtProcessQueueSize:    l.rtProcessResults.Len(),
					connectionsQueueSize:  l.connectionsResults.Len(),
					eventQueueSize:        l.eventResults.Len(),
					podQueueSize:          l.podResults.Len(),
					processQueueBytes:     l.processResults.Weight(),
					rtProcessQueueBytes:   l.rtProcessResults.Weight(),
					connectionsQueueBytes: l.connectionsResults.Weight(),
					eventQueueBytes:       l.eventResults.Weight(),
					podQueueBytes:         l.podResults.Weight(),
				})
			case <-queueLogTicker.C:
				l.logQueuesSize()
			case <-exit:
				return
			}
		}
	}()

	processForwarderOpts := forwarder.NewOptionsWithResolvers(resolver.NewSingleDomainResolvers(apicfg.KeysPerDomains(processAPIEndpoints)))
	processForwarderOpts.DisableAPIKeyChecking = true
	processForwarderOpts.RetryQueuePayloadsTotalMaxSize = l.forwarderRetryQueueMaxBytes // Allow more in-flight requests than the default
	processForwarder := forwarder.NewDefaultForwarder(processForwarderOpts)

	// rt forwarder reuses processForwarder's config
	rtProcessForwarder := forwarder.NewDefaultForwarder(processForwarderOpts)

	// connections forwarder reuses processForwarder's config
	connectionsForwarder := forwarder.NewDefaultForwarder(processForwarderOpts)

	podForwarderOpts := forwarder.NewOptionsWithResolvers(resolver.NewSingleDomainResolvers(apicfg.KeysPerDomains(l.cfg.Orchestrator.OrchestratorEndpoints)))
	podForwarderOpts.DisableAPIKeyChecking = true
	podForwarderOpts.RetryQueuePayloadsTotalMaxSize = l.forwarderRetryQueueMaxBytes // Allow more in-flight requests than the default
	podForwarder := forwarder.NewDefaultForwarder(podForwarderOpts)

	eventForwarderOpts := forwarder.NewOptionsWithResolvers(resolver.NewSingleDomainResolvers(apicfg.KeysPerDomains(processEventsAPIEndpoints)))
	eventForwarderOpts.DisableAPIKeyChecking = true
	eventForwarderOpts.RetryQueuePayloadsTotalMaxSize = l.forwarderRetryQueueMaxBytes // Allow more in-flight requests than the default
	eventForwarder := forwarder.NewDefaultForwarder(eventForwarderOpts)

	if err := processForwarder.Start(); err != nil {
		return fmt.Errorf("error starting forwarder: %s", err)
	}

	if err := rtProcessForwarder.Start(); err != nil {
		return fmt.Errorf("error starting RT forwarder: %s", err)
	}

	if err := connectionsForwarder.Start(); err != nil {
		return fmt.Errorf("error starting connections forwarder: %s", err)
	}

	if err := podForwarder.Start(); err != nil {
		return fmt.Errorf("error starting pod forwarder: %s", err)
	}

	if err := eventForwarder.Start(); err != nil {
		return fmt.Errorf("error starting event forwarder: %s", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(l.processResults, processForwarder)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(l.rtProcessResults, rtProcessForwarder)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(l.connectionsResults, connectionsForwarder)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(l.podResults, podForwarder)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		l.consumePayloads(l.eventResults, eventForwarder)
	}()

	for _, c := range l.enabledChecks {
		runner, err := l.runnerForCheck(c, exit)
		if err != nil {
			return fmt.Errorf("error starting check %s: %s", c.Name(), err)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			runner()
		}()
	}

	<-exit
	wg.Wait()

	for _, check := range l.enabledChecks {
		log.Debugf("Cleaning up %s check", check.Name())
		check.Cleanup()
	}

	processForwarder.Stop()
	rtProcessForwarder.Stop()
	connectionsForwarder.Stop()
	podForwarder.Stop()
	return nil
}

func (l *Collector) resultsQueueForCheck(name string) *api.WeightedQueue {
	switch name {
	case checks.Pod.Name():
		return l.podResults
	case checks.Process.RealTimeName(), checks.RTContainer.Name():
		return l.rtProcessResults
	case checks.Connections.Name():
		return l.connectionsResults
	case checks.ProcessEvents.Name():
		return l.eventResults
	}
	return l.processResults
}

func (l *Collector) runnerForCheck(c checks.Check, exit chan struct{}) (func(), error) {
	results := l.resultsQueueForCheck(c.Name())

	withRealTime, ok := c.(checks.CheckWithRealTime)
	if !l.runRealTime || !ok {
		return l.basicRunner(c, results, exit), nil
	}

	rtResults := l.resultsQueueForCheck(withRealTime.RealTimeName())

	return checks.NewRunnerWithRealTime(
		checks.RunnerConfig{
			CheckInterval: l.cfg.CheckInterval(withRealTime.Name()),
			RtInterval:    l.cfg.CheckInterval(withRealTime.RealTimeName()),

			ExitChan:       exit,
			RtIntervalChan: l.rtIntervalCh,
			RtEnabled: func() bool {
				return l.realTimeEnabled.Load()
			},
			RunCheck: func(options checks.RunOptions) {
				l.runCheckWithRealTime(withRealTime, results, rtResults, options)
			},
		},
	)
}

func (l *Collector) basicRunner(c checks.Check, results *api.WeightedQueue, exit chan struct{}) func() {
	return func() {
		// Run the check the first time to prime the caches.
		if !c.RealTime() {
			l.runCheck(c, results)
		}

		ticker := time.NewTicker(l.cfg.CheckInterval(c.Name()))
		for {
			select {
			case <-ticker.C:
				realTimeEnabled := l.runRealTime && l.realTimeEnabled.Load()
				if !c.RealTime() || realTimeEnabled {
					l.runCheck(c, results)
				}
			case d := <-l.rtIntervalCh:

				// Live-update the ticker.
				if c.RealTime() {
					ticker.Stop()
					ticker = time.NewTicker(d)
				}
			case _, ok := <-exit:
				if !ok {
					return
				}
			}
		}
	}
}

func (l *Collector) shouldDropPayload(check string) bool {
	for _, d := range l.dropCheckPayloads {
		if d == check {
			return true
		}
	}

	return false
}

func (l *Collector) consumePayloads(results *api.WeightedQueue, fwd forwarder.Forwarder) {
	for {
		// results.Poll() will return ok=false when stopped
		item, ok := results.Poll()
		if !ok {
			return
		}
		result := item.(*checkResult)
		for _, payload := range result.payloads {
			var (
				forwarderPayload = forwarder.Payloads{&payload.body}
				responses        chan forwarder.Response
				err              error
				updateRTStatus   = l.runRealTime
			)

			if l.shouldDropPayload(result.name) {
				continue
			}

			switch result.name {
			case checks.Process.Name():
				responses, err = fwd.SubmitProcessChecks(forwarderPayload, payload.headers)
			case checks.Process.RealTimeName():
				responses, err = fwd.SubmitRTProcessChecks(forwarderPayload, payload.headers)
			case checks.Container.Name():
				responses, err = fwd.SubmitContainerChecks(forwarderPayload, payload.headers)
			case checks.RTContainer.Name():
				responses, err = fwd.SubmitRTContainerChecks(forwarderPayload, payload.headers)
			case checks.Connections.Name():
				responses, err = fwd.SubmitConnectionChecks(forwarderPayload, payload.headers)
			case checks.Pod.Name():
				// Orchestrator intake response does not change RT checks enablement or interval
				updateRTStatus = false
				// Pod check contains two parts: metadata and manifest.
				// The manifest payload header has Content-Encoding:zstd allowing us to decompress payload in the intake
				if payload.headers.Get(headers.ContentEncodingHeader) == headers.ZSTDContentEncoding {
					responses, err = fwd.SubmitOrchestratorManifests(forwarderPayload, payload.headers)
				} else {
					responses, err = fwd.SubmitOrchestratorChecks(forwarderPayload, payload.headers, int(orchestrator.K8sPod))
				}
			case checks.ProcessDiscovery.Name():
				// A Process Discovery check does not change the RT mode
				updateRTStatus = false
				responses, err = fwd.SubmitProcessDiscoveryChecks(forwarderPayload, payload.headers)
			case checks.ProcessEvents.Name():
				updateRTStatus = false
				responses, err = fwd.SubmitProcessEventChecks(forwarderPayload, payload.headers)
			default:
				err = fmt.Errorf("unsupported payload type: %s", result.name)
			}

			if err != nil {
				log.Errorf("Unable to submit payload: %s", err)
				continue
			}

			if statuses := readResponseStatuses(result.name, responses); len(statuses) > 0 {
				if updateRTStatus {
					l.updateRTStatus(statuses)
				}
			}
		}
	}
}

func (l *Collector) updateRTStatus(statuses []*model.CollectorStatus) {
	curEnabled := l.realTimeEnabled.Load()

	// If any of the endpoints wants real-time we'll do that.
	// We will pick the maximum interval given since generally this is
	// only set if we're trying to limit load on the backend.
	shouldEnableRT := false
	maxInterval := 0 * time.Second
	activeClients := int32(0)
	for _, s := range statuses {
		shouldEnableRT = shouldEnableRT || s.ActiveClients > 0
		if s.ActiveClients > 0 {
			activeClients += s.ActiveClients
		}
		interval := time.Duration(s.Interval) * time.Second
		if interval > maxInterval {
			maxInterval = interval
		}
	}

	if curEnabled && !shouldEnableRT {
		log.Info("Detected 0 clients, disabling real-time mode")
		l.realTimeEnabled.Store(false)
	} else if !curEnabled && shouldEnableRT {
		log.Infof("Detected %d active clients, enabling real-time mode", activeClients)
		l.realTimeEnabled.Store(true)
	}

	if maxInterval != l.realTimeInterval {
		l.realTimeInterval = maxInterval
		if l.realTimeInterval <= 0 {
			l.realTimeInterval = 2 * time.Second
		}
		// Pass along the real-time interval, one per check, so that every
		// check routine will see the new interval.
		for range l.enabledChecks {
			l.rtIntervalCh <- l.realTimeInterval
		}
		log.Infof("real time interval updated to %s", l.realTimeInterval)
	}
}

func (l *Collector) logQueuesSize() {
	var (
		processSize     = l.processResults.Len()
		rtProcessSize   = l.rtProcessResults.Len()
		connectionsSize = l.connectionsResults.Len()
		eventsSize      = l.eventResults.Len()
		podSize         = l.podResults.Len()
	)

	if processSize == 0 &&
		rtProcessSize == 0 &&
		connectionsSize == 0 &&
		eventsSize == 0 &&
		podSize == 0 {
		return
	}

	log.Infof(
		"Delivery queues: process[size=%d, weight=%d], rtprocess[size=%d, weight=%d], connections[size=%d, weight=%d], event[size=%d, weight=%d], pod[size=%d, weight=%d]",
		processSize, l.processResults.Weight(),
		rtProcessSize, l.rtProcessResults.Weight(),
		connectionsSize, l.connectionsResults.Weight(),
		eventsSize, l.eventResults.Weight(),
		podSize, l.podResults.Weight(),
	)

}

// getContainerCount returns the number of containers in the message body
func getContainerCount(mb model.MessageBody) int {
	switch v := mb.(type) {
	case *model.CollectorProc:
		return len(v.GetContainers())
	case *model.CollectorRealTime:
		return len(v.GetContainerStats())
	case *model.CollectorContainer:
		return len(v.GetContainers())
	case *model.CollectorContainerRealTime:
		return len(v.GetStats())
	case *model.CollectorConnections:
		return 0
	}
	return 0
}

func readResponseStatuses(checkName string, responses <-chan forwarder.Response) []*model.CollectorStatus {
	var statuses []*model.CollectorStatus

	for response := range responses {
		if response.Err != nil {
			log.Errorf("[%s] Error from %s: %s", checkName, response.Domain, response.Err)
			continue
		}

		if response.StatusCode >= 300 {
			log.Errorf("[%s] Invalid response from %s: %d -> %s", checkName, response.Domain, response.StatusCode, response.Err)
			continue
		}

		// some checks don't receive a response with a status used to enable RT mode
		if ignoreResponseBody(checkName) {
			continue
		}

		r, err := model.DecodeMessage(response.Body)
		if err != nil {
			log.Errorf("[%s] Could not decode response body: %s", checkName, err)
			continue
		}

		switch r.Header.Type {
		case model.TypeResCollector:
			rm := r.Body.(*model.ResCollector)
			if len(rm.Message) > 0 {
				log.Errorf("[%s] Error in response from %s: %s", checkName, response.Domain, rm.Message)
			} else {
				statuses = append(statuses, rm.Status)
			}
		default:
			log.Errorf("[%s] Unexpected response type from %s: %d", checkName, response.Domain, r.Header.Type)
		}
	}

	return statuses
}

func ignoreResponseBody(checkName string) bool {
	switch checkName {
	case checks.Pod.Name(), checks.ProcessEvents.Name():
		return true
	default:
		return false
	}
}

// Pod check returns a list of messages can be divided into two parts : pod payloads and manifest payloads
// By default we only send pod payloads containing pod metadata and pod manifests (yaml)
// Manifest payloads is a copy of pod manifests, we only send manifest payloads when feature flag is true
func handlePodChecks(l *Collector, start time.Time, name string, messages []model.MessageBody, results *api.WeightedQueue) {
	l.messagesToResults(start, name, messages[:len(messages)/2], results)
	if l.cfg.Orchestrator.IsManifestCollectionEnabled {
		l.messagesToResults(start, name, messages[len(messages)/2:], results)
	}
}

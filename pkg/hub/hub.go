/*
Copyright (C) 2018 Synopsys, Inc.

Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownership. The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License. You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied. See the License for the
specific language governing permissions and limitations
under the License.
*/

package hub

import (
	"fmt"
	"time"

	"github.com/blackducksoftware/perceptor/pkg/api"
	"github.com/blackducksoftware/perceptor/pkg/util"
	log "github.com/sirupsen/logrus"
)

const (
	maxHubExponentialBackoffDuration = 1 * time.Hour
)

type clientAction struct {
	name  string
	apply func() error
}

// Hub .....
type Hub struct {
	client *Client
	// basic hub info
	host   string
	status ClientStatus
	// data
	model  *Model
	errors []error
	// timers
	getMetricsTimer              *util.Timer
	loginTimer                   *util.Timer
	refreshScansTimer            *util.Timer
	fetchAllScansTimer           *util.Timer
	fetchScansTimer              *util.Timer
	checkScansForCompletionTimer *util.Timer
	// public channels
	publishUpdatesCh chan Update
	// channels
	stop    chan struct{}
	actions chan *clientAction
}

// NewHub returns a new Hub.  It will not be logged in.
func NewHub(username string, password string, host string, rawClient RawClientInterface, timings *Timings) *Hub {
	hub := &Hub{
		client:           NewClient(username, password, host, rawClient),
		host:             host,
		status:           ClientStatusDown,
		errors:           []error{},
		publishUpdatesCh: make(chan Update),
		stop:             make(chan struct{}),
		actions:          make(chan *clientAction)}
	// timers
	hub.getMetricsTimer = hub.startGetMetricsTimer(timings.GetMetricsPause)
	hub.checkScansForCompletionTimer = hub.startCheckScansForCompletionTimer(timings.ScanCompletionPause)
	hub.fetchScansTimer = hub.startFetchUnknownScansTimer(timings.FetchUnknownScansPause)
	hub.fetchAllScansTimer = hub.startFetchAllScansTimer(timings.FetchAllScansPause)
	hub.loginTimer = hub.startLoginTimer(timings.LoginPause)
	hub.refreshScansTimer = hub.startRefreshScansTimer(timings.RefreshScanThreshold)
	// action processing
	go func() {
		for {
			select {
			case <-hub.stop:
				return
			case action := <-hub.actions:
				// TODO what other logging, metrics, etc. would help here?
				recordEvent(hub.host, action.name)
				err := action.apply()
				if err != nil {
					log.Errorf("while processing action %s: %s", action.name, err.Error())
					recordError(hub.host, action.name)
				}
			}
		}
	}()
	return hub
}

// Private methods

func (hub *Hub) publish(update Update) {
	go func() {
		select {
		case <-hub.stop:
			return
		case hub.publishUpdatesCh <- update:
		}
	}()
}

func (hub *Hub) getStateMetrics() {
	hub.model.getStateMetrics()
}

func (hub *Hub) recordError(err error) {
	if err != nil {
		hub.errors = append(hub.errors, err)
	}
	if len(hub.errors) > 1000 {
		hub.errors = hub.errors[500:]
	}
}

func (hub *Hub) apiModel() *api.ModelHub {
	errors := make([]string, len(hub.errors))
	for ix, err := range hub.errors {
		errors[ix] = err.Error()
	}
	apiModel := hub.model.apiModel()
	apiModel.Status = hub.status.String()
	apiModel.CircuitBreaker = hub.client.circuitBreaker.Model()
	return apiModel
}

// Regular jobs

func (hub *Hub) startRefreshScansTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("refresh-scans-%s", hub.host)
	return util.NewTimer(name, pause, hub.stop, func() {
		// TODO implement
	})
}

func (hub *Hub) didLogin(err error) {
	hub.actions <- &clientAction{"didLogin", func() error {
		hub.recordError(err)
		if err != nil && hub.status == ClientStatusUp {
			hub.status = ClientStatusDown
			hub.recordError(hub.checkScansForCompletionTimer.Pause())
			hub.recordError(hub.fetchScansTimer.Pause())
			hub.recordError(hub.fetchAllScansTimer.Pause())
			hub.recordError(hub.refreshScansTimer.Pause())
		} else if err == nil && hub.status == ClientStatusDown {
			hub.status = ClientStatusUp
			hub.recordError(hub.checkScansForCompletionTimer.Resume(true))
			hub.recordError(hub.fetchScansTimer.Resume(true))
			hub.recordError(hub.fetchAllScansTimer.Resume(true))
			hub.recordError(hub.refreshScansTimer.Resume(true))
		}
		return nil
	}}
}

func (hub *Hub) startLoginTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("login-%s", hub.host)
	return util.NewRunningTimer(name, pause, hub.stop, true, func() {
		log.Debugf("starting to login to hub")
		err := hub.client.login()
		hub.didLogin(err)
	})
}

func (hub *Hub) startFetchAllScansTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("fetchScans-%s", hub.host)
	return util.NewTimer(name, pause, hub.stop, func() {
		log.Debugf("starting to fetch all scans")
		cls, err := hub.client.listAllCodeLocations()
		hub.model.didFetchScans(cls, err)
	})
}

func (hub *Hub) startFetchUnknownScansTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("fetchUnknownScans-%s", hub.host)
	return util.NewTimer(name, pause, hub.stop, func() {
		log.Debugf("starting to fetch unknown scans")
		hub.model.fetchUnknownScans()
	})
}

func (hub *Hub) startGetMetricsTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("getMetrics-%s", hub.host)
	return util.NewRunningTimer(name, pause, hub.stop, true, func() {
		hub.getStateMetrics()
	})
}

func (hub *Hub) startCheckScansForCompletionTimer(pause time.Duration) *util.Timer {
	name := fmt.Sprintf("checkScansForCompletion-%s", hub.host)
	return util.NewTimer(name, pause, hub.stop, func() {
		hub.model.checkScansForCompletion()
	})
}

// Some public API methods ...

// StartScanClient ...
func (hub *Hub) StartScanClient(scanName string) {
	hub.model.StartScanClient(scanName)
}

// FinishScanClient ...
func (hub *Hub) FinishScanClient(scanName string, scanErr error) {
	hub.model.FinishScanClient(scanName, scanErr)
}

// ScansCount ...
func (hub *Hub) ScansCount() <-chan int {
	return hub.model.ScansCount()
}

// InProgressScans ...
func (hub *Hub) InProgressScans() <-chan []string {
	return hub.model.InProgressScans()
}

// ScanResults ...
func (hub *Hub) ScanResults() <-chan map[string]*Scan {
	return hub.model.ScanResults()
}

// Updates produces events for:
// - finding a scan for the first time
// - when a hub scan finishes
// - when a finished scan is repulled (to get any changes to its vulnerabilities, policies, etc.)
func (hub *Hub) Updates() <-chan Update {
	return hub.publishUpdatesCh
}

// Stop ...
func (hub *Hub) Stop() {
	close(hub.stop)
}

// StopCh returns a reference to the stop channel
func (hub *Hub) StopCh() <-chan struct{} {
	return hub.stop
}

// Host ...
func (hub *Hub) Host() string {
	return hub.host
}

// ResetCircuitBreaker ...
func (hub *Hub) ResetCircuitBreaker() {
	recordEvent(hub.host, "resetCircuitBreaker")
	hub.client.resetCircuitBreaker()
}

// Model ...
func (hub *Hub) Model() <-chan *api.ModelHub {
	ch := make(chan *api.ModelHub)
	hub.actions <- &clientAction{"getModel", func() error {
		ch <- hub.apiModel()
		return nil
	}}
	return ch
}

// HasFetchedScans ...
func (hub *Hub) HasFetchedScans() <-chan bool {
	return hub.model.HasFetchedScans()
}

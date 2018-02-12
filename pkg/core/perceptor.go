/*
Copyright (C) 2018 Black Duck Software, Inc.

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

package core

import (
	"sync"
	"time"

	api "github.com/blackducksoftware/perceptor/pkg/api"
	"github.com/blackducksoftware/perceptor/pkg/hub"
	log "github.com/sirupsen/logrus"
)

const (
	checkHubForCompletedScansPause = 20 * time.Second
	checkHubThrottle               = 1 * time.Second

	checkForStalledScansPause = 1 * time.Minute
	stalledScanTimeout        = 30 * time.Minute
)

// Perceptor ties together: a cluster, scan clients, and a hub.
// It listens to the cluster to learn about new pods.
// It keeps track of pods, containers, images, and scan results in a model.
// It has the hub scan images that have never been seen before.
// It grabs the scan results from the hub and adds them to its model.
// It publishes vulnerabilities that the cluster can find out about.
type Perceptor struct {
	hubClient     hub.FetcherInterface
	httpResponder *HTTPResponder
	// reducer
	reducer *reducer
	// channels
	actions                   chan action
	inProgressScanClientScans []*ImageInfo
	inProgressHubScans        []Image
}

// NewMockedPerceptor creates a Perceptor which uses a mock hub
func NewMockedPerceptor() (*Perceptor, error) {
	return newPerceptorHelper(hub.NewMockHub()), nil
}

// NewPerceptor creates a Perceptor using a real hub client.
func NewPerceptor(cfg *PerceptorConfig) (*Perceptor, error) {
	baseURL := "https://" + cfg.HubHost
	hubClient, err := hub.NewFetcher(cfg.HubUser, cfg.HubUserPassword, baseURL)
	if err != nil {
		log.Errorf("unable to instantiate hub Fetcher: %s", err.Error())
		return nil, err
	}

	return newPerceptorHelper(hubClient), nil
}

func newPerceptorHelper(hubClient hub.FetcherInterface) *Perceptor {
	// 0. prepare for circular communication
	model := make(chan Model)
	actions := make(chan action)

	metricsHandler := newMetrics()

	// 1. here's the responder
	httpResponder := NewHTTPResponder(model, metricsHandler)
	api.SetupHTTPServer(httpResponder)

	concurrentScanLimit := 2

	// 2. combine actions
	go func() {
		for {
			select {
			case pod := <-httpResponder.addPod:
				actions <- addPod{pod}
			case pod := <-httpResponder.updatePod:
				actions <- updatePod{pod}
			case podName := <-httpResponder.deletePod:
				actions <- deletePod{podName}
			case image := <-httpResponder.addImage:
				actions <- addImage{image}
			case pods := <-httpResponder.allPods:
				actions <- allPods{pods}
			case job := <-httpResponder.postFinishScanJob:
				actions <- finishScanClient{DockerImageSha(job.Sha), job.Err}
			case continuation := <-httpResponder.postNextImage:
				actions <- getNextImage{continuation}
			}
		}
	}()

	// 3. now for the reducer
	reducer := newReducer(*NewModel(concurrentScanLimit), actions)

	// 5. instantiate perceptor
	perceptor := Perceptor{
		hubClient:                 hubClient,
		httpResponder:             httpResponder,
		reducer:                   reducer,
		actions:                   actions,
		inProgressScanClientScans: []*ImageInfo{},
		inProgressHubScans:        []Image{},
	}

	// 4. close the circle
	go func() {
		for {
			select {
			case nextModel := <-reducer.model:
				perceptor.inProgressHubScans = nextModel.inProgressHubScans()
				perceptor.inProgressScanClientScans = nextModel.inProgressScanClientScans()
				metricsHandler.updateModel(nextModel)
				model <- nextModel
			}
		}
	}()

	// 7. hit the hub for results
	go perceptor.startCheckingForImagesInHub()
	go perceptor.startPollingHubForCompletedScans()

	// 8. done
	return &perceptor
}

func (perceptor *Perceptor) startCheckingForStalledScans() {
	for {
		time.Sleep(checkForStalledScansPause)
		for _, imageInfo := range perceptor.inProgressScanClientScans {
			if imageInfo.timeInCurrentScanStatus() > stalledScanTimeout {
				perceptor.actions <- requeueStalledScan{imageInfo.ImageSha}
			}
		}
	}
}

func (perceptor *Perceptor) startPollingHubForCompletedScans() {
	for {
		time.Sleep(checkHubForCompletedScansPause)

		for _, image := range perceptor.inProgressHubScans {
			scan, err := perceptor.hubClient.FetchScanFromImage(image)
			if err != nil {
				log.Errorf("check hub for completed scans -- unable to fetch image scan for image %s: %s", image.HubProjectName(), err.Error())
			} else {
				if scan == nil {
					log.Infof("check hub for completed scans -- unable to find image scan for image %s, found nil", image.HubProjectName())
				} else {
					log.Infof("check hub for completed scans -- found image scan for image %s: %%v", image.HubProjectName(), *scan)
				}
				perceptor.actions <- hubScanResults{HubImageScan{Sha: image.Sha, Scan: scan}}
			}
			time.Sleep(checkHubThrottle)
		}
	}
}

func (perceptor *Perceptor) startCheckingForImagesInHub() {
	for {
		var wg sync.WaitGroup
		wg.Add(1)
		var image *Image
		perceptor.actions <- getNextImageForHubPolling{func(i *Image) {
			image = i
			wg.Done()
		}}
		wg.Wait()

		if image != nil {
			scan, err := perceptor.hubClient.FetchScanFromImage(*image)
			if err != nil {
				log.Errorf("check images in hub -- unable to fetch image scan for image %s: %s", image.HubProjectName(), err.Error())
			} else {
				if scan == nil {
					log.Infof("check images in hub -- unable to find image scan for image %s, found nil", image.HubProjectName())
				} else {
					log.Infof("check images in hub -- found image scan for image %s: %+v", image.HubProjectName(), *scan)
				}
				perceptor.actions <- hubCheckResults{HubImageScan{Sha: (*image).Sha, Scan: scan}}
			}
			time.Sleep(checkHubThrottle)
		} else {
			// slow down the chatter if we didn't find something
			time.Sleep(checkHubForCompletedScansPause)
		}
	}
}

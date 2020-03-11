// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"go.universe.tf/metallb/internal/allocator"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/k8s"
	"go.universe.tf/metallb/internal/logging"
	"go.universe.tf/metallb/internal/version"

	"github.com/go-kit/kit/log"
	"k8s.io/api/core/v1"
)

// Service offers methods to mutate a Kubernetes service object.
type service interface {
	Update(svc *v1.Service) (*v1.Service, error)
	UpdateStatus(svc *v1.Service) error
	Infof(svc *v1.Service, desc, msg string, args ...interface{})
	Errorf(svc *v1.Service, desc, msg string, args ...interface{})
}

type controller struct {
	client service
	synced bool
	config *config.Config
	ips    *allocator.Allocator
}

func (c *controller) SetBalancer(l log.Logger, name string, svcRo *v1.Service, _ *v1.Endpoints) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of service update")
	defer l.Log("event", "endUpdate", "msg", "end of service update")

	if svcRo == nil {
		c.deleteBalancer(l, name)
		// There might be other LBs stuck waiting for an IP, so when
		// we delete a balancer we should reprocess all of them to
		// check for newly feasible balancers.
		return k8s.SyncStateReprocessAll
	}

	if c.config == nil {
		// Config hasn't been read, nothing we can do just yet.
		l.Log("event", "noConfig", "msg", "not processing, still waiting for config")
		return k8s.SyncStateSuccess
	}

	// Making a copy unconditionally is a bit wasteful, since we don't
	// always need to update the service. But, making an unconditional
	// copy makes the code much easier to follow, and we have a GC for
	// a reason.
	svc := svcRo.DeepCopy()
	if !c.convergeBalancer(l, name, svc) {
		return k8s.SyncStateError
	}
	if reflect.DeepEqual(svcRo, svc) {
		l.Log("event", "noChange", "msg", "service converged, no change")
		return k8s.SyncStateSuccess
	}

	var err error
	if !(reflect.DeepEqual(svcRo.Annotations, svc.Annotations) && reflect.DeepEqual(svcRo.Spec, svc.Spec)) {
		svcRo, err = c.client.Update(svc)
		if err != nil {
			l.Log("op", "updateService", "error", err, "msg", "failed to update service")
			return k8s.SyncStateError
		}
	}
	if !reflect.DeepEqual(svcRo.Status, svc.Status) {
		var st v1.ServiceStatus
		st, svc = svc.Status, svcRo.DeepCopy()
		svc.Status = st
		if err = c.client.UpdateStatus(svc); err != nil {
			l.Log("op", "updateServiceStatus", "error", err, "msg", "failed to update service status")
			return k8s.SyncStateError
		}
	}
	l.Log("event", "serviceUpdated", "msg", "updated service object")

	return k8s.SyncStateSuccess
}

func (c *controller) deleteBalancer(l log.Logger, name string) {
	if c.ips.Unassign(name) {
		l.Log("event", "serviceDeleted", "msg", "service deleted")
	}
}

func (c *controller) SetConfig(l log.Logger, cfg *config.Config) k8s.SyncState {
	l.Log("event", "startUpdate", "msg", "start of config update")
	defer l.Log("event", "endUpdate", "msg", "end of config update")

	if cfg == nil {
		l.Log("op", "setConfig", "error", "no MetalLB configuration in cluster", "msg", "configuration is missing, MetalLB will not function")
		return k8s.SyncStateError
	}

	if err := c.ips.SetPools(cfg.Pools); err != nil {
		l.Log("op", "setConfig", "error", err, "msg", "applying new configuration failed")
		return k8s.SyncStateError
	}
	c.config = cfg
	return k8s.SyncStateReprocessAll
}

func (c *controller) MarkSynced(l log.Logger) {
	c.synced = true
	l.Log("event", "stateSynced", "msg", "controller synced, can allocate IPs now")
}

func main() {
	logger, err := logging.Init()
	if err != nil {
		fmt.Printf("failed to initialize logging: %s\n", err)
		os.Exit(1)
	}

	var (
		port   = flag.Int("port", 7472, "HTTP listening port for Prometheus metrics")
		config = flag.String("config", "config", "Kubernetes ConfigMap containing MetalLB's configuration")
	)
	flag.Parse()

	logger.Log("version", version.Version(), "commit", version.CommitHash(), "branch", version.Branch(), "msg", "MetalLB controller starting "+version.String())

	c := &controller{
		ips: allocator.New(),
	}

	client, err := k8s.New(&k8s.Config{
		ProcessName:   "metallb-controller",
		ConfigMapName: *config,
		MetricsPort:   *port,
		Logger:        logger,

		ServiceChanged: c.SetBalancer,
		ConfigChanged:  c.SetConfig,
		Synced:         c.MarkSynced,
	})
	if err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to create k8s client")
		os.Exit(1)
	}

	c.client = client
	if err := client.Run(nil); err != nil {
		logger.Log("op", "startup", "error", err, "msg", "failed to run k8s client")
	}
}

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
	"errors"
	"fmt"
	"net"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"

	"go.universe.tf/metallb/internal/config"
)

func (c *controller) convergeBalancer(key string, svc *v1.Service) error {
	var lbIP net.IP

	// Not a LoadBalancer, early exit. It might have been a balancer
	// in the past, so we still need to clear LB state.
	if svc.Spec.Type != "LoadBalancer" {
		glog.Infof("%s: not a LoadBalancer, clearing assignment", key)
		c.clearServiceState(key, svc)
		// Early return, we explicitly do *not* want to reallocate
		// an IP.
		return nil
	}

	// The assigned LB IP is the end state of convergence. If there's
	// none or a malformed one, nuke all controlled state so that we
	// start converging from a clean slate.
	if len(svc.Status.LoadBalancer.Ingress) == 1 {
		lbIP = net.ParseIP(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if lbIP == nil {
		glog.Infof("%s: currently has no ingress IP", key)
		c.clearServiceState(key, svc)
	}

	// It's possible the config mutated and the IP we have no longer
	// makes sense. If so, clear it out and give the rest of the logic
	// a chance to allocate again.
	if lbIP != nil {
		// This assign is idempotent if the config is consistent,
		// otherwise it'll fail and tell us why.
		if err := c.ips.Assign(key, lbIP); err != nil {
			glog.Infof("%s: clearing assignment %q, %s", key, lbIP)
			c.clearServiceState(key, svc)
			lbIP = nil
		}
	}

	// User set or changed the desired LB IP, nuke the
	// state. allocateIP will pay attention to LoadBalancerIP and try
	// to meet the user's demands.
	if svc.Spec.LoadBalancerIP != "" && svc.Spec.LoadBalancerIP != lbIP.String() {
		glog.Infof("%s: clearing assignment %q, user requested %q", key, lbIP, svc.Spec.LoadBalancerIP)
		c.clearServiceState(key, svc)
		lbIP = nil
	}

	// If lbIP is still nil at this point, try to allocate.
	if lbIP == nil {
		if !c.synced {
			glog.Infof("%s: not allocating IP yet, controller not synced", key)
			return errors.New("not allocating IPs yet, not synced")
		}
		glog.Infof("%s: needs an IP, allocating", key)
		ip, err := c.allocateIP(key, svc)
		if err != nil {
			glog.Infof("%s: allocation failed: %s", key, err)
			c.client.Errorf(svc, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
			// TODO: should retry on pool exhaustion allocation
			// failures, once we keep track of when pools become
			// non-full.
			return nil
		}
		lbIP = ip
		glog.Infof("%s: allocated %q", key, lbIP)
		c.client.Infof(svc, "IPAllocated", "Assigned IP %q", lbIP)
	}

	if lbIP == nil {
		glog.Infof("%s: failed to allocate an IP, but did not exit convergeService early (BUG!)", key)
		c.client.Errorf(svc, "InternalError", "didn't allocate an IP but also did not fail")
		c.clearServiceState(key, svc)
		return nil
	}

	pool := c.ips.Pool(key)
	if pool == "" || c.config.Pools[pool] == nil {
		glog.Infof("%s: allocated IP has no matching pool (BUG!)", key)
		c.client.Errorf(svc, "InternalError", "allocated an IP that has no pool")
		c.clearServiceState(key, svc)
		return nil
	}

	if c.config.Pools[pool].Protocol == config.ARP {
		// When advertising in ARP mode, any node in the cluster could
		// become the leader in charge of advertising the IP. The
		// local traffic policy makes no sense for such services, so
		// we force the service to be load-balanced at the cluster
		// scope.
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
	}

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}
	return nil
}

// clearServiceState clears all fields that are actively managed by
// this controller.
func (c *controller) clearServiceState(key string, svc *v1.Service) {
	c.ips.Unassign(key)
	svc.Status.LoadBalancer = v1.LoadBalancerStatus{}
}

func (c *controller) allocateIP(key string, svc *v1.Service) (net.IP, error) {
	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {
		ip := net.ParseIP(svc.Spec.LoadBalancerIP).To4()
		if ip == nil {
			return nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}
		if err := c.ips.Assign(key, ip); err != nil {
			return nil, err
		}
		return ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	desiredPool := svc.Annotations["metallb.universe.tf/address-pool"]
	if desiredPool != "" {
		ip, err := c.ips.AllocateFromPool(key, desiredPool)
		if err != nil {
			return nil, err
		}
		return ip, nil
	}

	// Okay, in that case just bruteforce across all pools.
	return c.ips.Allocate(key)
}

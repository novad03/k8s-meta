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
	"fmt"
	"io"
	"net"
	"reflect"
	"sort"
	"time"

	"go.universe.tf/metallb/internal/bgp"
	"go.universe.tf/metallb/internal/config"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/golang/glog"
)

type peer struct {
	cfg *config.Peer
	bgp session
}

type bgpController struct {
	myIP       net.IP
	nodeLabels labels.Set
	peers      []*peer
	svcAds     map[string][]*bgp.Advertisement
}

func (c *bgpController) SetConfig(cfg *config.Config) error {
	newPeers := make([]*peer, 0, len(cfg.Peers))
newPeers:
	for _, p := range cfg.Peers {
		for i, ep := range c.peers {
			if ep == nil {
				continue
			}
			if reflect.DeepEqual(p, ep.cfg) {
				newPeers = append(newPeers, ep)
				c.peers[i] = nil
				continue newPeers
			}
		}
		// No existing peers match, create a new one.
		newPeers = append(newPeers, &peer{
			cfg: p,
		})
	}

	oldPeers := c.peers
	c.peers = newPeers

	for _, p := range oldPeers {
		if p == nil {
			continue
		}
		glog.Infof("Peer %q deconfigured, closing BGP session", p.cfg.Addr)
		if p.bgp != nil {
			if err := p.bgp.Close(); err != nil {
				glog.Warningf("Shutting down BGP session to %q: %s", p.cfg.Addr, err)
			}
		}
	}

	return c.syncPeers()
}

// Called when either the peer list or node labels have changed,
// implying that the set of running BGP sessions may need tweaking.
func (c *bgpController) syncPeers() error {
	var (
		errs          []error
		needUpdateAds bool
	)
	for _, p := range c.peers {
		// First, determine if the peering should be active for this
		// node.
		shouldRun := false
		for _, ns := range p.cfg.NodeSelectors {
			if ns.Matches(c.nodeLabels) {
				shouldRun = true
				break
			}
		}

		// Now, compare current state to intended state, and correct.
		if p.bgp != nil && !shouldRun {
			// Oops, session is running but shouldn't be. Shut it down.
			glog.Infof("Peer %q deconfigured, stopping BGP session", p.cfg.Addr)
			if err := p.bgp.Close(); err != nil {
				glog.Warningf("Shutting down BGP session to %q: %s", p.cfg.Addr, err)
			}
			p.bgp = nil
		} else if p.bgp == nil && shouldRun {
			// Session doesn't exist, but should be running. Create
			// it.
			glog.Infof("Peer %q configured, starting BGP session", p.cfg.Addr)
			routerID := c.myIP
			if p.cfg.RouterID != nil {
				routerID = p.cfg.RouterID
			}
			s, err := newBGP(fmt.Sprintf("%s:%d", p.cfg.Addr, p.cfg.Port), p.cfg.MyASN, routerID, p.cfg.ASN, p.cfg.HoldTime)
			if err != nil {
				errs = append(errs, fmt.Errorf("Creating BGP session to %q: %s", p.cfg.Addr, err))
			} else {
				p.bgp = s
				needUpdateAds = true
			}
		}
	}
	if needUpdateAds {
		// Some new sessions came up, resync advertisement state.
		if err := c.updateAds(); err != nil {
			return err
		}
	}
	if len(errs) != 0 {
		for _, err := range errs {
			glog.Error(err)
		}
		return fmt.Errorf("%d BGP sessions failed to start", len(errs))
	}
	return nil
}

func (c *bgpController) SetBalancer(name string, lbIP net.IP, pool *config.Pool) error {
	c.svcAds[name] = nil
	for _, adCfg := range pool.BGPAdvertisements {
		m := net.CIDRMask(adCfg.AggregationLength, 32)
		ad := &bgp.Advertisement{
			Prefix: &net.IPNet{
				IP:   lbIP.Mask(m),
				Mask: m,
			},
			NextHop:   c.myIP,
			LocalPref: adCfg.LocalPref,
		}
		for comm := range adCfg.Communities {
			ad.Communities = append(ad.Communities, comm)
		}
		sort.Slice(ad.Communities, func(i, j int) bool { return ad.Communities[i] < ad.Communities[j] })
		c.svcAds[name] = append(c.svcAds[name], ad)
	}

	if err := c.updateAds(); err != nil {
		return err
	}

	glog.Infof("%s: making %d advertisements using BGP", name, len(c.svcAds[name]))

	return nil
}

func (c *bgpController) updateAds() error {
	var allAds []*bgp.Advertisement
	for _, ads := range c.svcAds {
		// This list might contain duplicates, but that's fine,
		// they'll get compacted by the session code when it's
		// calculating advertisements.
		//
		// TODO: be more intelligent about compacting advertisements
		// and detecting conflicting advertisements.
		allAds = append(allAds, ads...)
	}
	for _, peer := range c.peers {
		if peer.bgp == nil {
			continue
		}
		if err := peer.bgp.Set(allAds...); err != nil {
			return err
		}
	}
	return nil
}

func (c *bgpController) DeleteBalancer(name, reason string) error {
	if _, ok := c.svcAds[name]; !ok {
		return nil
	}
	delete(c.svcAds, name)
	return c.updateAds()
}

type session interface {
	io.Closer
	Set(advs ...*bgp.Advertisement) error
}

func (c *bgpController) SetLeader(bool) {}

func (c *bgpController) SetNode(node *v1.Node) error {
	nodeLabels := node.Labels
	if nodeLabels == nil {
		nodeLabels = map[string]string{}
	}
	ns := labels.Set(nodeLabels)
	if c.nodeLabels != nil && labels.Equals(c.nodeLabels, ns) {
		// Node labels unchanged, no action required.
		return nil
	}
	c.nodeLabels = ns
	glog.Infof("Node labels changed, resyncing BGP peers")
	return c.syncPeers()
}

var newBGP = func(addr string, myASN uint32, routerID net.IP, asn uint32, hold time.Duration) (session, error) {
	return bgp.New(addr, myASN, routerID, asn, hold)
}

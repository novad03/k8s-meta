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
	"net"
	"os"

	"go.universe.tf/metallb/internal/k8s"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
)

var announcing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "metallb",
	Subsystem: "speaker",
	Name:      "announced",
	Help:      "Services being announced from this node. This is desired state, it does not guarantee that the routing protocols have converged.",
}, []string{
	"service",
	"protocol",
	"node",
	"ip",
})

func main() {
	prometheus.MustRegister(announcing)

	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	master := flag.String("master", "", "master url")
	myIPstr := flag.String("node-ip", "", "IP address of this Kubernetes node")
	myNode := flag.String("node-name", "", "name of this Kubernetes node")
	port := flag.Int("port", 80, "HTTP listening port")
	flag.Parse()

	if *myIPstr == "" {
		*myIPstr = os.Getenv("METALLB_NODE_IP")
	}
	if *myNode == "" {
		*myNode = os.Getenv("METALLB_NODE_NAME")
	}

	myIP := net.ParseIP(*myIPstr).To4()
	if myIP == nil {
		glog.Fatalf("Invalid --node-ip %q, must be an IPv4 address", *myIPstr)
	}

	if *myNode == "" {
		glog.Fatalf("Must specify --node-name")
	}

	// Setup both ARP and BGP clients and speakers, config decides what is being done runtime.

	cBGP, err := newBGPController(myIP, *myNode)
	if err != nil {
		glog.Fatalf("Error getting BGP controller: %s", err)
	}
	cARP, err := newARPController(myIP, *myNode)
	if err != nil {
		glog.Fatalf("Error getting ARP controller: %s", err)
	}

	client, err := k8s.NewClient(speaker, *master, *kubeconfig, true, cBGP, cARP)
	if err != nil {
		glog.Fatalf("Error getting k8s client: %s", err)
	}

	le, err := client.NewLeaderElector(cARP.ann, speaker)
	if err != nil {
		glog.Fatalf("Error setting up leader election: %s", err)
	}
	go func() { le.Run() }()

	glog.Fatal(client.Run(*port))
}

const speaker = "metallb-speaker"

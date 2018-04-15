package layer2

import (
	"time"
)

// Leader returns true if we are the leader in the daemon set.
func (a *Announce) Leader() bool {
	a.RLock()
	defer a.RUnlock()
	return a.leader
}

// SetLeader sets the leader boolean to b.
func (a *Announce) SetLeader(b bool) {
	a.Lock()
	defer a.Unlock()
	a.leader = b
	if a.leader {
		go a.Acquire()
	}
}

// Acquire sends out a unsolicited ARP replies for all VIPs that should be announced.
func (a *Announce) Acquire() {
	go a.spam()
}

// spam broadcasts unsolicited ARP replies for 5 seconds.
func (a *Announce) spam() {
	start := time.Now()
	for time.Since(start) < 5*time.Second {

		if !a.Leader() {
			return
		}

		for _, ip := range a.ips {
			if err := a.gratuitous(ip); err != nil {
				a.logger.Log("op", "sendGratuitous", "ip", ip, "error", err, "msg", "failed to send gratuitous layer 2 response for IP")
			}
		}
		time.Sleep(1100 * time.Millisecond)
	}
}

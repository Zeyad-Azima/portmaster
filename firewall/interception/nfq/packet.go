// +build linux

package nfq

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/tevino/abool"

	"github.com/florianl/go-nfqueue"
	"github.com/safing/portbase/log"
	pmpacket "github.com/safing/portmaster/network/packet"
)

// Firewalling marks used by the Portmaster.
// See TODO on packet.mark() on their relevance
// and a possibility to remove most IPtables rules.
const (
	MarkAccept       = 1700
	MarkBlock        = 1701
	MarkDrop         = 1702
	MarkAcceptAlways = 1710
	MarkBlockAlways  = 1711
	MarkDropAlways   = 1712
	MarkRerouteNS    = 1799
	MarkRerouteSPN   = 1717
)

func markToString(mark int) string {
	switch mark {
	case MarkAccept:
		return "Accept"
	case MarkBlock:
		return "Block"
	case MarkDrop:
		return "Drop"
	case MarkAcceptAlways:
		return "AcceptAlways"
	case MarkBlockAlways:
		return "BlockAlways"
	case MarkDropAlways:
		return "DropAlways"
	case MarkRerouteNS:
		return "RerouteNS"
	case MarkRerouteSPN:
		return "RerouteSPN"
	}
	return "unknown"
}

// packet implements the packet.Packet interface.
type packet struct {
	pmpacket.Base
	pktID          uint32
	received       time.Time
	queue          *Queue
	verdictSet     chan struct{}
	verdictPending *abool.AtomicBool
}

func (pkt *packet) ID() string {
	return fmt.Sprintf("pkt:%d qid:%d", pkt.pktID, pkt.queue.id)
}

// TODO(ppacher): revisit the following behavior:
// 		The legacy implementation of nfqueue (and the interception) module
// 		always accept a packet but may mark it so that a subsequent rule in
// 		the C17 chain drops, rejects or modifies it.
//
//		For drop/return we could use the actual nfQueue verdicts Drop and Stop.
//		Re-routing to local NS or SPN can be done by modifying the packet here
//		and using SetVerdictModPacket and reject can be implemented using a simple
// 		raw-socket.
//
func (pkt *packet) mark(mark int) (err error) {
	if pkt.verdictPending.SetToIf(false, true) {
		defer close(pkt.verdictSet)
		return pkt.setMark(mark)
	}

	return errors.New("verdict set")
}

func (pkt *packet) setMark(mark int) error {
	atomic.AddUint64(&pkt.queue.pendingVerdicts, 1)

	defer func() {
		atomic.AddUint64(&pkt.queue.pendingVerdicts, ^uint64(0))
		select {
		case pkt.queue.verdictCompleted <- struct{}{}:
		default:
		}
	}()

	for {
		if err := pkt.queue.nf.SetVerdictWithMark(pkt.pktID, nfqueue.NfAccept, mark); err != nil {
			// embedded interface is required to work-around some
			// dep-vendoring weirdness
			if opErr, ok := err.(interface {
				Timeout() bool
				Temporary() bool
			}); ok {
				if opErr.Timeout() || opErr.Temporary() {
					continue
				}
			}

			log.Errorf("nfqueue: failed to set verdict %s for %s (%s -> %s): %s", markToString(mark), pkt.ID(), pkt.Info().Src, pkt.Info().Dst, err)
			return err
		}
		break
	}
	log.Tracef("nfqueue: marking packet %s (%s -> %s) on queue %d with %s after %s", pkt.ID(), pkt.Info().Src, pkt.Info().Dst, pkt.queue.id, markToString(mark), time.Since(pkt.received))
	return nil
}

func (pkt *packet) Accept() error {
	return pkt.mark(MarkAccept)
}

func (pkt *packet) Block() error {
	if pkt.Info().Protocol == pmpacket.ICMP {
		// ICMP packets attributed to a blocked connection are always allowed, as
		// rejection ICMP packets will have the same mark as the blocked
		// connection. This is why we need to drop blocked ICMP packets instead.
		return pkt.mark(MarkDrop)
	}
	return pkt.mark(MarkBlock)
}

func (pkt *packet) Drop() error {
	return pkt.mark(MarkDrop)
}

func (pkt *packet) PermanentAccept() error {
	return pkt.mark(MarkAcceptAlways)
}

func (pkt *packet) PermanentBlock() error {
	if pkt.Info().Protocol == pmpacket.ICMP {
		// ICMP packets attributed to a blocked connection are always allowed, as
		// rejection ICMP packets will have the same mark as the blocked
		// connection. This is why we need to drop blocked ICMP packets instead.
		return pkt.mark(MarkDropAlways)
	}
	return pkt.mark(MarkBlockAlways)
}

func (pkt *packet) PermanentDrop() error {
	return pkt.mark(MarkDropAlways)
}

func (pkt *packet) RerouteToNameserver() error {
	return pkt.mark(MarkRerouteNS)
}

func (pkt *packet) RerouteToTunnel() error {
	return pkt.mark(MarkRerouteSPN)
}

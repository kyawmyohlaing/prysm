package sync

import (
	"context"
	"encoding/hex"
	"sort"
	"time"

	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/sync/peerstatus"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/traceutil"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
	"golang.org/x/exp/rand"
)

var processPendingBlocksPeriod = time.Duration(params.BeaconConfig().SecondsPerSlot/3) * time.Second

// processes pending blocks queue on every processPendingBlocksPeriod
func (r *RegularSync) processPendingBlocksQueue() {
	ticker := time.NewTicker(processPendingBlocksPeriod)
	for {
		ctx := context.TODO()
		select {
		case <-ticker.C:
			r.processPendingBlocks(ctx)
		case <-r.ctx.Done():
			log.Debug("Context closed, exiting routine")
			return
		}
	}
}

// processes the block tree inside the queue
func (r *RegularSync) processPendingBlocks(ctx context.Context) error {
	ctx, span := trace.StartSpan(ctx, "processPendingBlocks")
	defer span.End()

	pids := peerstatus.Keys()
	slots := r.sortedPendingSlots()

	span.AddAttributes(
		trace.Int64Attribute("numSlots", int64(len(slots))),
		trace.Int64Attribute("numPeers", int64(len(pids))),
	)

	for _, s := range slots {
		ctx, span := trace.StartSpan(ctx, "processPendingBlocks.InnerLoop")
		span.AddAttributes(trace.Int64Attribute("slot", int64(s)))

		r.pendingQueueLock.RLock()
		b := r.slotToPendingBlocks[uint64(s)]
		inPendingQueue := r.seenPendingBlocks[bytesutil.ToBytes32(b.ParentRoot)]
		r.pendingQueueLock.RUnlock()

		inDB := r.db.HasBlock(ctx, bytesutil.ToBytes32(b.ParentRoot))
		hasPeer := len(pids) != 0

		// Only request for missing parent block if it's not in DB, not in pending cache
		// and has peer in the peer list.
		if !inPendingQueue && !inDB && hasPeer {
			log.WithFields(logrus.Fields{
				"currentSlot": b.Slot,
				"parentRoot":  hex.EncodeToString(b.ParentRoot),
			}).Info("Requesting parent block")
			req := [][32]byte{bytesutil.ToBytes32(b.ParentRoot)}
			if err := r.sendRecentBeaconBlocksRequest(ctx, req, pids[rand.Int()%len(pids)]); err != nil {
				traceutil.AnnotateError(span, err)
				log.Errorf("Could not send recent block request: %v", err)
			}
			span.End()
			continue
		}

		if !inDB {
			span.End()
			continue
		}

		if err := r.chain.ReceiveBlockNoPubsub(ctx, b); err != nil {
			log.Errorf("Could not process block from slot %d: %v", b.Slot, err)
			traceutil.AnnotateError(span, err)
		}

		r.pendingQueueLock.Lock()
		delete(r.slotToPendingBlocks, uint64(s))
		blkRoot, err := ssz.SigningRoot(b)
		if err != nil {
			traceutil.AnnotateError(span, err)
			span.End()
			return err
		}
		delete(r.seenPendingBlocks, blkRoot)
		r.pendingQueueLock.Unlock()

		log.Infof("Processed ancestor block with slot %d and cleared pending block cache", s)
	}
	return nil
}

func (r *RegularSync) sortedPendingSlots() []int {
	r.pendingQueueLock.RLock()
	defer r.pendingQueueLock.RUnlock()

	slots := make([]int, 0, len(r.slotToPendingBlocks))
	for s := range r.slotToPendingBlocks {
		slots = append(slots, int(s))
	}
	sort.Ints(slots)

	return slots
}

package sync

import (
	"context"
	"io"
	"time"

	libp2pcore "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/beacon-chain/state/stateutil"
	pbp2p "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/params"
)

// sendRecentBeaconBlocksRequest sends a recent beacon blocks request to a peer to get
// those corresponding blocks from that peer.
func (r *Service) sendRecentBeaconBlocksRequest(ctx context.Context, blockRoots [][]byte, id peer.ID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := &pbp2p.BeaconBlocksByRootRequest{BlockRoots: blockRoots}
	stream, err := r.p2p.Send(ctx, req, p2p.RPCBlocksByRootTopic, id)
	if err != nil {
		return err
	}
	defer func() {
		if err := stream.Reset(); err != nil {
			log.WithError(err).Errorf("Failed to reset stream with protocol %s", stream.Protocol())
		}
	}()
	for i := 0; i < len(blockRoots); i++ {
		blk, err := ReadChunkedBlock(stream, r.p2p)
		if err == io.EOF {
			break
		}
		// Exit if peer sends more than max request blocks.
		if uint64(i) >= params.BeaconNetworkConfig().MaxRequestBlocks {
			break
		}
		if err != nil {
			log.WithError(err).Error("Unable to retrieve block from stream")
			return err
		}

		blkRoot, err := stateutil.BlockRoot(blk.Block)
		if err != nil {
			return err
		}
		r.pendingQueueLock.Lock()
		r.slotToPendingBlocks[blk.Block.Slot] = blk
		r.seenPendingBlocks[blkRoot] = true
		r.pendingQueueLock.Unlock()

	}
	return nil
}

// beaconBlocksRootRPCHandler looks up the request blocks from the database from the given block roots.
func (r *Service) beaconBlocksRootRPCHandler(ctx context.Context, msg interface{}, stream libp2pcore.Stream) error {
	defer func() {
		if err := stream.Close(); err != nil {
			log.WithError(err).Error("Failed to close stream")
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	setRPCStreamDeadlines(stream)
	log := log.WithField("handler", "beacon_blocks_by_root")

	req, ok := msg.(*pbp2p.BeaconBlocksByRootRequest)
	if !ok {
		return errors.New("message is not type BeaconBlocksByRootRequest")
	}
	if len(req.BlockRoots) == 0 {
		resp, err := r.generateErrorResponse(responseCodeInvalidRequest, "no block roots provided in request")
		if err != nil {
			log.WithError(err).Error("Failed to generate a response error")
		} else {
			if _, err := stream.Write(resp); err != nil {
				log.WithError(err).Errorf("Failed to write to stream")
			}
		}
		return errors.New("no block roots provided")
	}

	if int64(len(req.BlockRoots)) > r.blocksRateLimiter.Remaining(stream.Conn().RemotePeer().String()) {
		r.p2p.Peers().IncrementBadResponses(stream.Conn().RemotePeer())
		if r.p2p.Peers().IsBad(stream.Conn().RemotePeer()) {
			log.Debug("Disconnecting bad peer")
			defer func() {
				if err := r.p2p.Disconnect(stream.Conn().RemotePeer()); err != nil {
					log.WithError(err).Error("Failed to disconnect peer")
				}
			}()
		}
		resp, err := r.generateErrorResponse(responseCodeInvalidRequest, rateLimitedError)
		if err != nil {
			log.WithError(err).Error("Failed to generate a response error")
		} else {
			if _, err := stream.Write(resp); err != nil {
				log.WithError(err).Errorf("Failed to write to stream")
			}
		}
		return errors.New(rateLimitedError)
	}

	if uint64(len(req.BlockRoots)) > params.BeaconNetworkConfig().MaxRequestBlocks {
		resp, err := r.generateErrorResponse(responseCodeInvalidRequest, "requested more than the max block limit")
		if err != nil {
			log.WithError(err).Error("Failed to generate a response error")
		} else {
			if _, err := stream.Write(resp); err != nil {
				log.WithError(err).Errorf("Failed to write to stream")
			}
		}
		return errors.New("requested more than the max block limit")
	}

	r.blocksRateLimiter.Add(stream.Conn().RemotePeer().String(), int64(len(req.BlockRoots)))

	for _, root := range req.BlockRoots {
		blk, err := r.db.Block(ctx, bytesutil.ToBytes32(root))
		if err != nil {
			log.WithError(err).Error("Failed to fetch block")
			resp, err := r.generateErrorResponse(responseCodeServerError, genericError)
			if err != nil {
				log.WithError(err).Error("Failed to generate a response error")
			} else {
				if _, err := stream.Write(resp); err != nil {
					log.WithError(err).Errorf("Failed to write to stream")
				}
			}
			return err
		}
		if blk == nil {
			continue
		}
		if err := r.chunkWriter(stream, blk); err != nil {
			return err
		}
	}
	return nil
}

// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package blocksync

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/libp2p/go-libp2p-core/peer"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/blockchain/block"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/consensus"
	"github.com/iotexproject/iotex-core/pkg/lifecycle"
	"github.com/iotexproject/iotex-core/pkg/log"
	"github.com/iotexproject/iotex-core/pkg/routine"
	"github.com/iotexproject/iotex-proto/golang/iotexrpc"
)

type (
	// UnicastOutbound sends a unicast message to the given address
	UnicastOutbound func(ctx context.Context, peer peer.AddrInfo, msg proto.Message) error
	// Neighbors returns the neighbors' addresses
	Neighbors func(ctx context.Context) ([]peer.AddrInfo, error)
)

// BlockDAO represents the block data access object
type BlockDAO interface {
	GetBlockByHeight(uint64) (*block.Block, error)
}

// Config represents the config to setup blocksync
type Config struct {
	unicastHandler   UnicastOutbound
	neighborsHandler Neighbors
}

// Option is the option to override the blocksync config
type Option func(cfg *Config) error

// WithUnicastOutBound is the option to set the unicast callback
func WithUnicastOutBound(unicastHandler UnicastOutbound) Option {
	return func(cfg *Config) error {
		cfg.unicastHandler = unicastHandler
		return nil
	}
}

// WithNeighbors is the option to set the neighbors callback
func WithNeighbors(neighborsHandler Neighbors) Option {
	return func(cfg *Config) error {
		cfg.neighborsHandler = neighborsHandler
		return nil
	}
}

// BlockSync defines the interface of blocksyncer
type BlockSync interface {
	lifecycle.StartStopper

	TargetHeight() uint64
	ProcessSyncRequest(ctx context.Context, peer peer.AddrInfo, sync *iotexrpc.BlockSync) error
	ProcessBlock(ctx context.Context, blk *block.Block) error
	ProcessBlockSync(ctx context.Context, blk *block.Block) error
	SyncStatus() string
}

// blockSyncer implements BlockSync interface
type blockSyncer struct {
	commitHeight          uint64 // last commit block height
	processSyncRequestTTL time.Duration
	buf                   *blockBuffer
	worker                *syncWorker
	bc                    blockchain.Blockchain
	dao                   BlockDAO
	unicastHandler        UnicastOutbound
	neighborsHandler      Neighbors
	syncStageTask         *routine.RecurringTask
	syncStageHeight       uint64
	syncBlockIncrease     uint64
}

// NewBlockSyncer returns a new block syncer instance
func NewBlockSyncer(
	cfg config.Config,
	chain blockchain.Blockchain,
	dao BlockDAO,
	cs consensus.Consensus,
	opts ...Option,
) (BlockSync, error) {
	buf := &blockBuffer{
		blocks:       make(map[uint64]*block.Block),
		bc:           chain,
		cs:           cs,
		bufferSize:   cfg.BlockSync.BufferSize,
		intervalSize: cfg.BlockSync.IntervalSize,
	}
	bsCfg := Config{}
	for _, opt := range opts {
		if err := opt(&bsCfg); err != nil {
			return nil, err
		}
	}
	bs := &blockSyncer{
		bc:                    chain,
		dao:                   dao,
		buf:                   buf,
		unicastHandler:        bsCfg.unicastHandler,
		neighborsHandler:      bsCfg.neighborsHandler,
		worker:                newSyncWorker(chain.ChainID(), cfg, bsCfg.unicastHandler, bsCfg.neighborsHandler, buf),
		processSyncRequestTTL: cfg.BlockSync.ProcessSyncRequestTTL,
	}
	bs.syncStageTask = routine.NewRecurringTask(bs.syncStageChecker, config.DardanellesBlockInterval)
	atomic.StoreUint64(&bs.syncBlockIncrease, 0)
	return bs, nil
}

// TargetHeight returns the target height to sync to
func (bs *blockSyncer) TargetHeight() uint64 {
	bs.worker.mu.RLock()
	defer bs.worker.mu.RUnlock()
	return bs.worker.targetHeight
}

// Start starts a block syncer
func (bs *blockSyncer) Start(ctx context.Context) error {
	log.L().Debug("Starting block syncer.")
	if err := bs.syncStageTask.Start(ctx); err != nil {
		return err
	}
	bs.commitHeight = bs.buf.CommitHeight()
	return bs.worker.Start(ctx)
}

// Stop stops a block syncer
func (bs *blockSyncer) Stop(ctx context.Context) error {
	log.L().Debug("Stopping block syncer.")
	if err := bs.syncStageTask.Stop(ctx); err != nil {
		return err
	}
	return bs.worker.Stop(ctx)
}

// ProcessBlock processes an incoming latest committed block
func (bs *blockSyncer) ProcessBlock(_ context.Context, blk *block.Block) error {
	var needSync bool
	moved, re := bs.buf.Flush(blk)
	switch re {
	case bCheckinLower:
		log.L().Debug("Drop block lower than buffer's accept height.")
	case bCheckinExisting:
		log.L().Debug("Drop block exists in buffer.")
	case bCheckinHigher:
		needSync = true
	case bCheckinValid:
		needSync = !moved
	case bCheckinSkipNil:
		needSync = false
	}

	if needSync {
		bs.worker.SetTargetHeight(blk.Height())
	}
	return nil
}

func (bs *blockSyncer) ProcessBlockSync(_ context.Context, blk *block.Block) error {
	bs.buf.Flush(blk)
	if bs.bc.TipHeight() == bs.TargetHeight() {
		bs.worker.SetTargetHeight(bs.TargetHeight() + bs.buf.bufSize())
	}
	return nil
}

// ProcessSyncRequest processes a block sync request
func (bs *blockSyncer) ProcessSyncRequest(ctx context.Context, peer peer.AddrInfo, sync *iotexrpc.BlockSync) error {
	end := bs.bc.TipHeight()
	switch {
	case sync.End < end:
		end = sync.End
	case sync.End > end:
		log.L().Debug(
			"Do not have requested blocks",
			zap.String("peerID", peer.ID.Pretty()),
			zap.Uint64("start", sync.Start),
			zap.Uint64("end", sync.End),
			zap.Uint64("tipHeight", end),
		)
	}
	for i := sync.Start; i <= end; i++ {
		blk, err := bs.dao.GetBlockByHeight(i)
		if err != nil {
			return err
		}
		// TODO: send back multiple blocks in one shot
		syncCtx, cancel := context.WithTimeout(ctx, bs.processSyncRequestTTL)
		defer cancel()
		if err := bs.unicastHandler(syncCtx, peer, blk.ConvertToBlockPb()); err != nil {
			log.L().Debug("Failed to response to ProcessSyncRequest.", zap.Error(err))
		}
	}
	return nil
}

func (bs *blockSyncer) syncStageChecker() {
	tipHeight := bs.bc.TipHeight()
	atomic.StoreUint64(&bs.syncBlockIncrease, tipHeight-bs.syncStageHeight)
	bs.syncStageHeight = tipHeight
}

// SyncStatus report block sync status
func (bs *blockSyncer) SyncStatus() string {
	syncBlockIncrease := atomic.LoadUint64(&bs.syncBlockIncrease)
	if syncBlockIncrease == 1 {
		return "synced to blockchain tip"
	}
	return fmt.Sprintf("sync in progress at %.1f blocks/sec", float64(syncBlockIncrease)/config.DardanellesBlockInterval.Seconds())
}

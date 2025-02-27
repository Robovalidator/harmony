package downloader

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/event"
	"github.com/harmony-one/harmony/core"
	nodeconfig "github.com/harmony-one/harmony/internal/configs/node"
	"github.com/harmony-one/harmony/internal/utils"
	"github.com/harmony-one/harmony/p2p"
	"github.com/harmony-one/harmony/p2p/stream/common/streammanager"
	"github.com/harmony-one/harmony/p2p/stream/protocols/sync"
	"github.com/rs/zerolog"
)

type (
	// Downloader is responsible for sync task of one shard
	Downloader struct {
		bc           blockChain
		ih           insertHelper
		syncProtocol syncProtocol
		bh           *beaconHelper

		downloadC chan struct{}
		closeC    chan struct{}
		ctx       context.Context
		cancel    func()

		evtDownloadFinished event.Feed // channel for each download task finished
		evtDownloadStarted  event.Feed // channel for each download has started

		status status
		config Config
		logger zerolog.Logger
	}
)

// NewDownloader creates a new downloader
func NewDownloader(host p2p.Host, bc *core.BlockChain, config Config) *Downloader {
	config.fixValues()

	ih := newInsertHelper(bc)

	sp := sync.NewProtocol(sync.Config{
		Chain:     bc,
		Host:      host.GetP2PHost(),
		Discovery: host.GetDiscovery(),
		ShardID:   nodeconfig.ShardID(bc.ShardID()),
		Network:   config.Network,

		SmSoftLowCap: config.SmSoftLowCap,
		SmHardLowCap: config.SmHardLowCap,
		SmHiCap:      config.SmHiCap,
		DiscBatch:    config.SmDiscBatch,
	})
	host.AddStreamProtocol(sp)

	var bh *beaconHelper
	if config.BHConfig != nil && bc.ShardID() == 0 {
		bh = newBeaconHelper(bc, ih, config.BHConfig.BlockC, config.BHConfig.InsertHook)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Downloader{
		bc:           bc,
		ih:           ih,
		syncProtocol: sp,
		bh:           bh,

		downloadC: make(chan struct{}),
		closeC:    make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,

		status: newStatus(),
		config: config,
		logger: utils.Logger().With().Str("module", "downloader").Logger(),
	}
}

// Start start the downloader
func (d *Downloader) Start() {
	if d.config.ServerOnly {
		return
	}

	go d.run()

	if d.bh != nil {
		d.bh.start()
	}
}

// Close close the downloader
func (d *Downloader) Close() {
	if d.config.ServerOnly {
		return
	}

	close(d.closeC)
	d.cancel()

	if d.bh != nil {
		d.bh.close()
	}
}

// DownloadAsync triggers the download async.
func (d *Downloader) DownloadAsync() {
	select {
	case d.downloadC <- struct{}{}:
		consensusTriggeredDownloadCounterVec.With(d.promLabels()).Inc()

	case <-time.After(100 * time.Millisecond):
	}
}

// NumPeers returns the number of peers connected of a specific shard.
func (d *Downloader) NumPeers() int {
	return d.syncProtocol.NumStreams()
}

// IsSyncing return the current sync status
func (d *Downloader) SyncStatus() (bool, uint64) {
	syncing, target := d.status.get()
	if !syncing {
		target = d.bc.CurrentBlock().NumberU64()
	}
	return syncing, target
}

// SubscribeDownloadStarted subscribe download started
func (d *Downloader) SubscribeDownloadStarted(ch chan struct{}) event.Subscription {
	return d.evtDownloadStarted.Subscribe(ch)
}

// SubscribeDownloadFinishedEvent subscribe the download finished
func (d *Downloader) SubscribeDownloadFinished(ch chan struct{}) event.Subscription {
	return d.evtDownloadFinished.Subscribe(ch)
}

func (d *Downloader) run() {
	d.waitForBootFinish()
	d.loop()
}

// waitForBootFinish wait for stream manager to finish the initial discovery and have
// enough peers to start downloader
func (d *Downloader) waitForBootFinish() {
	evtCh := make(chan streammanager.EvtStreamAdded, 1)
	sub := d.syncProtocol.SubscribeAddStreamEvent(evtCh)
	defer sub.Unsubscribe()

	checkCh := make(chan struct{}, 1)
	trigger := func() {
		select {
		case checkCh <- struct{}{}:
		default:
		}
	}
	trigger()

	t := time.NewTicker(10 * time.Second)

	for {
		d.logger.Info().Msg("waiting for initial bootstrap discovery")
		select {
		case <-t.C:
			trigger()

		case <-evtCh:
			trigger()

		case <-checkCh:
			if d.syncProtocol.NumStreams() >= d.config.InitStreams {
				return
			}
		case <-d.closeC:
			return
		}
	}
}

func (d *Downloader) loop() {
	ticker := time.NewTicker(10 * time.Second)
	initSync := true
	trigger := func() {
		select {
		case d.downloadC <- struct{}{}:
		case <-time.After(100 * time.Millisecond):
		}
	}
	go trigger()

	for {
		select {
		case <-ticker.C:
			go trigger()

		case <-d.downloadC:
			addedBN, err := d.doDownload(initSync)
			if err != nil {
				// If error happens, sleep 5 seconds and retry
				d.logger.Warn().Err(err).Bool("bootstrap", initSync).Msg("failed to download")
				go func() {
					time.Sleep(5 * time.Second)
					trigger()
				}()
				continue
			}
			d.logger.Info().Int("block added", addedBN).
				Uint64("current height", d.bc.CurrentBlock().NumberU64()).
				Bool("initSync", initSync).
				Uint32("shard", d.bc.ShardID()).
				Msg("sync finished")

			if addedBN != 0 {
				// If block number has been changed, trigger another sync
				// and try to add last mile from pub-sub (blocking)
				go trigger()
				if d.bh != nil {
					d.bh.insertSync()
				}
			}
			initSync = false

		case <-d.closeC:
			return
		}
	}
}

func (d *Downloader) doDownload(initSync bool) (n int, err error) {
	if initSync {
		d.logger.Info().Uint64("current number", d.bc.CurrentBlock().NumberU64()).
			Uint32("shard ID", d.bc.ShardID()).Msg("start long range sync")

		n, err = d.doLongRangeSync()
	} else {
		d.logger.Info().Uint64("current number", d.bc.CurrentBlock().NumberU64()).
			Uint32("shard ID", d.bc.ShardID()).Msg("start short range sync")

		n, err = d.doShortRangeSync()
	}
	if err != nil {
		pl := d.promLabels()
		pl["error"] = err.Error()
		numFailedDownloadCounterVec.With(pl).Inc()
		return
	}
	return
}

func (d *Downloader) startSyncing() {
	d.status.startSyncing()
	d.evtDownloadStarted.Send(struct{}{})
}

func (d *Downloader) finishSyncing() {
	d.status.finishSyncing()
	d.evtDownloadFinished.Send(struct{}{})
}

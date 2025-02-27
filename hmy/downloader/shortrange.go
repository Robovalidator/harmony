package downloader

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/harmony-one/harmony/core/types"
	syncProto "github.com/harmony-one/harmony/p2p/stream/protocols/sync"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

var emptySigVerifyError *sigVerifyError

// doShortRangeSync does the short range sync.
// Compared with long range sync, short range sync is more focused on syncing to the latest block.
// It consist of 3 steps:
// 1. Obtain the block hashes and ompute the longest hash chain..
// 2. Get blocks by hashes from computed hash chain.
// 3. Insert the blocks to blockchain.
func (d *Downloader) doShortRangeSync() (int, error) {
	numShortRangeCounterVec.With(d.promLabels()).Inc()

	sh := &srHelper{
		syncProtocol: d.syncProtocol,
		ctx:          d.ctx,
		config:       d.config,
		logger:       d.logger.With().Str("mode", "short range").Logger(),
	}

	if err := sh.checkPrerequisites(); err != nil {
		return 0, errors.Wrap(err, "prerequisite")
	}
	curBN := d.bc.CurrentBlock().NumberU64()
	hashChain, whitelist, err := sh.getHashChain(curBN)
	if err != nil {
		return 0, errors.Wrap(err, "getHashChain")
	}
	if len(hashChain) == 0 {
		// short circuit for no sync is needed
		return 0, nil
	}

	d.startSyncing()
	expEndBN := curBN + uint64(len(hashChain)) - 1
	d.status.setTargetBN(expEndBN)
	defer d.finishSyncing()

	blocks, err := sh.getBlocksByHashes(hashChain, whitelist)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			sh.removeStreams(whitelist) // Remote nodes cannot provide blocks with target hashes
		}
		return 0, errors.Wrap(err, "getBlocksByHashes")
	}

	n, err := d.ih.verifyAndInsertBlocks(blocks)
	numBlocksInsertedShortRangeHistogramVec.With(d.promLabels()).Observe(float64(n))
	if err != nil {
		if !errors.As(err, &emptySigVerifyError) {
			sh.removeStreams(whitelist) // Data provided by remote nodes is corrupted
		}
		return n, errors.Wrap(err, "InsertChain")
	}
	return len(blocks), nil
}

type srHelper struct {
	syncProtocol syncProtocol

	ctx    context.Context
	config Config
	logger zerolog.Logger
}

func (sh *srHelper) getHashChain(curBN uint64) ([]common.Hash, []sttypes.StreamID, error) {
	bns := sh.prepareBlockHashNumbers(curBN)
	results := newBlockHashResults(bns)

	var wg sync.WaitGroup
	wg.Add(sh.config.Concurrency)

	for i := 0; i != sh.config.Concurrency; i++ {
		go func() {
			defer wg.Done()

			hashes, stid, err := sh.doGetBlockHashesRequest(bns)
			if err != nil {
				return
			}
			results.addResult(hashes, stid)
		}()
	}
	wg.Wait()

	select {
	case <-sh.ctx.Done():
		return nil, nil, sh.ctx.Err()
	default:
	}

	hashChain, wl := results.computeLongestHashChain()
	return hashChain, wl, nil
}

func (sh *srHelper) getBlocksByHashes(hashes []common.Hash, whitelist []sttypes.StreamID) ([]*types.Block, error) {
	ctx, cancel := context.WithCancel(sh.ctx)
	m := newGetBlocksByHashManager(hashes, whitelist)

	var (
		wg      sync.WaitGroup
		gErr    error
		errLock sync.Mutex
	)

	wg.Add(sh.config.Concurrency)
	for i := 0; i != sh.config.Concurrency; i++ {
		go func() {
			defer wg.Done()
			defer cancel() // it's ok to cancel context more than once

			for {
				if m.isDone() {
					return
				}
				hashes, wl, err := m.getNextHashes()
				if err != nil {
					errLock.Lock()
					gErr = err
					errLock.Unlock()
					return
				}
				if len(hashes) == 0 {
					select {
					case <-time.After(200 * time.Millisecond):
						continue
					case <-ctx.Done():
						return
					}
				}
				blocks, stid, err := sh.doGetBlocksByHashesRequest(ctx, hashes, wl)
				if err != nil {
					sh.logger.Err(err).Msg("getBlocksByHashes worker failed")
					m.handleResultError(hashes, stid)
				} else {
					m.addResult(hashes, blocks, stid)
				}
			}
		}()
	}
	wg.Wait()

	if gErr != nil {
		return nil, gErr
	}
	select {
	case <-sh.ctx.Done():
		return nil, sh.ctx.Err()
	default:
	}

	return m.getResults()
}

func (sh *srHelper) checkPrerequisites() error {
	if sh.syncProtocol.NumStreams() < sh.config.Concurrency {
		return errors.New("not enough streams")
	}
	return nil
}

func (sh *srHelper) prepareBlockHashNumbers(curNumber uint64) []uint64 {
	res := make([]uint64, 0, numBlockHashesPerRequest)

	for bn := curNumber + 1; bn <= curNumber+uint64(numBlockHashesPerRequest); bn++ {
		res = append(res, bn)
	}
	return res
}

func (sh *srHelper) doGetBlockHashesRequest(bns []uint64) ([]common.Hash, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(sh.ctx, 1*time.Second)
	defer cancel()

	hashes, stid, err := sh.syncProtocol.GetBlockHashes(ctx, bns)
	if err != nil {
		return nil, stid, err
	}
	if len(hashes) != len(bns) {
		err := errors.New("unexpected get block hashes result delivered")
		sh.logger.Warn().Err(err).Str("stream", string(stid)).Msg("failed to doGetBlockHashesRequest")
		sh.syncProtocol.RemoveStream(stid)
		return nil, stid, err
	}
	return hashes, stid, nil
}

func (sh *srHelper) doGetBlocksByHashesRequest(ctx context.Context, hashes []common.Hash, wl []sttypes.StreamID) ([]*types.Block, sttypes.StreamID, error) {
	ctx, cancel := context.WithTimeout(sh.ctx, 10*time.Second)
	defer cancel()

	blocks, stid, err := sh.syncProtocol.GetBlocksByHashes(ctx, hashes,
		syncProto.WithWhitelist(wl))
	if err != nil {
		return nil, stid, err
	}
	if err := checkGetBlockByHashesResult(blocks, hashes); err != nil {
		sh.logger.Warn().Err(err).Str("stream", string(stid)).Msg("failed to getBlockByHashes")
		sh.syncProtocol.RemoveStream(stid)
		return nil, stid, err
	}
	return blocks, stid, nil
}

func (sh *srHelper) removeStreams(sts []sttypes.StreamID) {
	for _, st := range sts {
		sh.syncProtocol.RemoveStream(st)
	}
}

func checkGetBlockByHashesResult(blocks []*types.Block, hashes []common.Hash) error {
	if len(blocks) != len(hashes) {
		return errors.New("unexpected number of getBlocksByHashes result")
	}
	for i, block := range blocks {
		if block == nil {
			return errors.New("nil block found")
		}
		if block.Hash() != hashes[i] {
			return fmt.Errorf("unexpected block hash: %x / %x", block.Hash(), hashes[i])
		}
	}
	return nil
}

type (
	blockHashResults struct {
		bns     []uint64
		results []map[sttypes.StreamID]common.Hash

		lock sync.Mutex
	}
)

func newBlockHashResults(bns []uint64) *blockHashResults {
	results := make([]map[sttypes.StreamID]common.Hash, 0, len(bns))
	for range bns {
		results = append(results, make(map[sttypes.StreamID]common.Hash))
	}
	return &blockHashResults{
		bns:     bns,
		results: results,
	}
}

func (res *blockHashResults) addResult(hashes []common.Hash, stid sttypes.StreamID) {
	res.lock.Lock()
	defer res.lock.Unlock()

	for i, h := range hashes {
		if h == emptyHash {
			return // nil block hash reached
		}
		res.results[i][stid] = h
	}
	return
}

func (res *blockHashResults) computeLongestHashChain() ([]common.Hash, []sttypes.StreamID) {
	var (
		whitelist map[sttypes.StreamID]struct{}
		hashChain []common.Hash
	)
	for _, result := range res.results {
		hash, nextWl := countHashMaxVote(result, whitelist)
		if hash == emptyHash {
			break
		}
		hashChain = append(hashChain, hash)
		whitelist = nextWl
	}

	sts := make([]sttypes.StreamID, 0, len(whitelist))
	for st := range whitelist {
		sts = append(sts, st)
	}
	return hashChain, sts
}

func countHashMaxVote(m map[sttypes.StreamID]common.Hash, whitelist map[sttypes.StreamID]struct{}) (common.Hash, map[sttypes.StreamID]struct{}) {
	var (
		voteM  = make(map[common.Hash]int)
		res    common.Hash
		maxCnt = 0
	)

	for st, h := range m {
		if len(whitelist) != 0 {
			if _, ok := whitelist[st]; !ok {
				continue
			}
		}
		if _, ok := voteM[h]; !ok {
			voteM[h] = 0
		}
		voteM[h]++
		if voteM[h] > maxCnt {
			maxCnt = voteM[h]
			res = h
		}
	}

	nextWl := make(map[sttypes.StreamID]struct{})
	for st, h := range m {
		if h != res {
			continue
		}
		if len(whitelist) != 0 {
			if _, ok := whitelist[st]; ok {
				nextWl[st] = struct{}{}
			}
		} else {
			nextWl[st] = struct{}{}
		}
	}
	return res, nextWl
}

type getBlocksByHashManager struct {
	hashes    []common.Hash
	pendings  map[common.Hash]struct{}
	results   map[common.Hash]blockResult
	whitelist []sttypes.StreamID

	lock sync.Mutex
}

func newGetBlocksByHashManager(hashes []common.Hash, whitelist []sttypes.StreamID) *getBlocksByHashManager {
	return &getBlocksByHashManager{
		hashes:    hashes,
		pendings:  make(map[common.Hash]struct{}),
		results:   make(map[common.Hash]blockResult),
		whitelist: whitelist,
	}
}

func (m *getBlocksByHashManager) getNextHashes() ([]common.Hash, []sttypes.StreamID, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	num := m.numBlocksPerRequest()
	hashes := make([]common.Hash, 0, num)
	if len(m.whitelist) == 0 {
		return nil, nil, errors.New("empty white list")
	}

	for _, hash := range m.hashes {
		if len(hashes) == num {
			break
		}
		_, ok1 := m.pendings[hash]
		_, ok2 := m.results[hash]
		if !ok1 && !ok2 {
			hashes = append(hashes, hash)
		}
	}
	sts := make([]sttypes.StreamID, len(m.whitelist))
	copy(sts, m.whitelist)
	return hashes, sts, nil
}

func (m *getBlocksByHashManager) numBlocksPerRequest() int {
	val := divideCeil(len(m.hashes), len(m.whitelist))
	if val < numBlocksByHashesLowerCap {
		val = numBlocksByHashesLowerCap
	}
	if val > numBlocksByHashesUpperCap {
		val = numBlocksByHashesUpperCap
	}
	return val
}

func (m *getBlocksByHashManager) addResult(hashes []common.Hash, blocks []*types.Block, stid sttypes.StreamID) {
	m.lock.Lock()
	defer m.lock.Unlock()

	for i, hash := range hashes {
		block := blocks[i]
		delete(m.pendings, hash)
		m.results[hash] = blockResult{
			block: block,
			stid:  stid,
		}
	}
}

func (m *getBlocksByHashManager) handleResultError(hashes []common.Hash, stid sttypes.StreamID) {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.removeStreamID(stid)

	for _, hash := range hashes {
		delete(m.pendings, hash)
	}
}

func (m *getBlocksByHashManager) getResults() ([]*types.Block, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	blocks := make([]*types.Block, 0, len(m.hashes))
	for _, hash := range m.hashes {
		if m.results[hash].block == nil {
			return nil, errors.New("SANITY: nil block found")
		}
		blocks = append(blocks, m.results[hash].block)
	}
	return blocks, nil
}

func (m *getBlocksByHashManager) isDone() bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return len(m.results) == len(m.hashes)
}

func (m *getBlocksByHashManager) removeStreamID(target sttypes.StreamID) {
	// O(n^2) complexity. But considering the whitelist size is small, should not
	// have performance issue.
loop:
	for i, stid := range m.whitelist {
		if stid == target {
			if i == len(m.whitelist) {
				m.whitelist = m.whitelist[:i]
			} else {
				m.whitelist = append(m.whitelist[:i], m.whitelist[i+1:]...)
			}
			goto loop
		}
	}
	return
}

func divideCeil(x, y int) int {
	fVal := float64(x) / float64(y)
	return int(math.Ceil(fVal))
}

package stagedstreamsync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/harmony-one/harmony/core"
	"github.com/harmony-one/harmony/internal/utils"
	sttypes "github.com/harmony-one/harmony/p2p/stream/types"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/log/v3"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

const (
	BlockHashesBucket            = "BlockHashes"
	BeaconBlockHashesBucket      = "BeaconBlockHashes"
	DownloadedBlocksBucket       = "BlockBodies"
	BeaconDownloadedBlocksBucket = "BeaconBlockBodies" // Beacon Block bodies are downloaded, TxHash and UncleHash are getting verified
	LastMileBlocksBucket         = "LastMileBlocks"    // last mile blocks to catch up with the consensus
	StageProgressBucket          = "StageProgress"

	// cache db keys
	LastBlockHeight = "LastBlockHeight"
	LastBlockHash   = "LastBlockHash"
)

var Buckets = []string{
	BlockHashesBucket,
	BeaconBlockHashesBucket,
	DownloadedBlocksBucket,
	BeaconDownloadedBlocksBucket,
	LastMileBlocksBucket,
	StageProgressBucket,
}

// CreateStagedSync creates an instance of staged sync
func CreateStagedSync(bc core.BlockChain,
	UseMemDB bool,
	protocol syncProtocol,
	config Config,
	logger zerolog.Logger,
	logProgress bool,
) (*StagedStreamSync, error) {

	isBeacon := bc.ShardID() == bc.Engine().Beaconchain().ShardID()

	var db kv.RwDB
	if UseMemDB {
		db = memdb.New()
	} else {
		if isBeacon {
			db = mdbx.NewMDBX(log.New()).Path("cache_beacon_db").MustOpen()
		} else {
			db = mdbx.NewMDBX(log.New()).Path("cache_shard_db").MustOpen()
		}
	}

	ctx := context.Background()

	if errInitDB := initDB(ctx, db); errInitDB != nil {
		return nil, errInitDB
	}

	stageHeadsCfg := NewStageHeadersCfg(ctx, bc, db)
	stageShortRangeCfg := NewStageShortRangeCfg(ctx, bc, db)
	stageSyncEpochCfg := NewStageEpochCfg(ctx, bc, db)
	stageBodiesCfg := NewStageBodiesCfg(ctx, bc, db, isBeacon, logProgress)
	stageStatesCfg := NewStageStatesCfg(ctx, bc, db, logger, logProgress)
	stageFinishCfg := NewStageFinishCfg(ctx, db)

	stages := DefaultStages(ctx,
		stageHeadsCfg,
		stageShortRangeCfg,
		stageSyncEpochCfg,
		stageBodiesCfg,
		stageStatesCfg,
		stageFinishCfg,
	)

	return New(ctx,
		bc,
		db,
		stages,
		isBeacon,
		protocol,
		UseMemDB,
		config,
		logger,
	), nil
}

// init sync loop main database and create buckets
func initDB(ctx context.Context, db kv.RwDB) error {
	tx, errRW := db.BeginRw(ctx)
	if errRW != nil {
		return errRW
	}
	defer tx.Rollback()
	for _, name := range Buckets {
		// create bucket
		if err := tx.CreateBucket(GetStageName(name, false, false)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// doSync does the long range sync.
// One LongRangeSync consists of several iterations.
// For each iteration, estimate the current block number, then fetch block & insert to blockchain
func (s *StagedStreamSync) doSync(downloaderContext context.Context, initSync bool) (int, error) {

	var totalInserted int

	s.initSync = initSync

	if err := s.checkPrerequisites(); err != nil {
		return 0, err
	}

	var estimatedHeight uint64
	if initSync {
		if h, err := s.estimateCurrentNumber(); err != nil {
			return 0, err
		} else {
			estimatedHeight = h
			//TODO: use directly currentCycle var
			s.status.setTargetBN(estimatedHeight)
		}

		if curBN := s.bc.CurrentBlock().NumberU64(); estimatedHeight <= curBN {
			s.logger.Info().Uint64("current number", curBN).Uint64("target number", estimatedHeight).
				Msg("early return of long range sync")
			return 0, nil
		}

		s.startSyncing()
		defer s.finishSyncing()

		utils.Logger().Info().
			Uint64("current height", s.bc.CurrentBlock().NumberU64()).
			Uint64("target height", estimatedHeight).
			Msgf("staged sync is executing ... ")
	}

	for {
		ctx, cancel := context.WithCancel(downloaderContext)
		s.ctx = ctx
		s.SetNewContext(ctx)

		n, err := s.doSyncCycle(ctx, initSync)
		if err != nil {
			pl := s.promLabels()
			pl["error"] = err.Error()
			numFailedDownloadCounterVec.With(pl).Inc()

			cancel()
			return totalInserted + n, err
		}
		cancel()

		totalInserted += n

		// if it's not long range sync, skip loop
		if n < lastMileThres || !initSync {
			return totalInserted, nil
		}
	}

}

func (s *StagedStreamSync) doSyncCycle(ctx context.Context, initSync bool) (int, error) {

	// TODO: initSync=true means currentCycleNumber==0, so we can remove initSync

	var totalInserted int

	//for {
	s.inserted = 0
	//ctx, cancel := context.WithCancel(s.ctx)
	//s.ctx = ctx
	startHead := s.bc.CurrentBlock().NumberU64()
	canRunCycleInOneTransaction := false

	var tx kv.RwTx
	if canRunCycleInOneTransaction {
		var err error
		if tx, err = s.DB().BeginRw(context.Background()); err != nil {
			return totalInserted, err
		}
		defer tx.Rollback()
	}

	startTime := time.Now()

	// Do one cycle of staged sync
	initialCycle := s.currentCycle.Number == 0
	if err := s.Run(s.DB(), tx, initialCycle); err != nil {
		// cancel()
		utils.Logger().Error().
			Err(err).
			Bool("isBeacon", s.isBeacon).
			Uint32("shard", s.bc.ShardID()).
			Uint64("currentHeight", startHead).
			Msgf("[STAGED_SYNC] sync cycle failed")
		return totalInserted, err
	}
	//cancel()

	totalInserted += s.inserted

	s.currentCycle.lock.Lock()
	s.currentCycle.Number++
	s.currentCycle.lock.Unlock()

	// calculating sync speed (blocks/second)
	//currHead := s.bc.CurrentBlock().NumberU64()
	if s.LogProgress && s.inserted > 0 {
		dt := time.Now().Sub(startTime).Seconds()
		speed := float64(0)
		if dt > 0 {
			speed = float64(s.inserted) / dt
		}
		syncSpeed := fmt.Sprintf("%.2f", speed)
		fmt.Println("sync speed:", syncSpeed, "blocks/s")
	}

	// if s.inserted < lastMileThres || !initSync {
	// 	return totalInserted, nil
	// }
	//}
	return totalInserted, nil
}

func (s *StagedStreamSync) startSyncing() {
	s.status.startSyncing()
	if s.evtDownloadStartedSubscribed {
		s.evtDownloadStarted.Send(struct{}{})
	}
}

func (s *StagedStreamSync) finishSyncing() {
	s.status.finishSyncing()
	if s.evtDownloadFinishedSubscribed {
		s.evtDownloadFinished.Send(struct{}{})
	}
}

func (s *StagedStreamSync) checkPrerequisites() error {
	return s.checkHaveEnoughStreams()
}

// estimateCurrentNumber roughly estimate the current block number.
// The block number does not need to be exact, but just a temporary target of the iteration
func (s *StagedStreamSync) estimateCurrentNumber() (uint64, error) {
	var (
		cnResults = make(map[sttypes.StreamID]uint64)
		lock      sync.Mutex
		wg        sync.WaitGroup
	)
	wg.Add(s.config.Concurrency)
	for i := 0; i != s.config.Concurrency; i++ {
		go func() {
			defer wg.Done()
			bn, stid, err := s.doGetCurrentNumberRequest()
			if err != nil {
				s.logger.Err(err).Str("streamID", string(stid)).
					Msg("getCurrentNumber request failed. Removing stream")
				if !errors.Is(err, context.Canceled) {
					s.protocol.RemoveStream(stid)
				}
				return
			}
			lock.Lock()
			cnResults[stid] = bn
			lock.Unlock()
		}()
	}
	wg.Wait()

	if len(cnResults) == 0 {
		select {
		case <-s.ctx.Done():
			return 0, s.ctx.Err()
		default:
		}
		return 0, errors.New("zero block number response from remote nodes")
	}
	bn := computeBlockNumberByMaxVote(cnResults)
	return bn, nil
}

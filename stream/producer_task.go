package stream

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/ava-labs/ortelius/services/indexes/models"

	"github.com/ava-labs/ortelius/services"
	"github.com/ava-labs/ortelius/services/db"
	"github.com/ava-labs/ortelius/services/indexes/params"
	"github.com/gocraft/dbr/v2"

	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/ortelius/cfg"
)

var (
	aggregationTick      = 20 * time.Second
	aggregateDeleteFrame = (-1 * 24 * 366) * time.Hour
	timestampRollup      = 60
	aggregateColumns     = []string{
		fmt.Sprintf("FROM_UNIXTIME(floor(UNIX_TIMESTAMP(avm_outputs.created_at) / %d) * %d) as aggregate_ts", timestampRollup, timestampRollup),
		"avm_outputs.asset_id",
		"CAST(COALESCE(SUM(avm_outputs.amount), 0) AS CHAR) AS transaction_volume",
		"COUNT(DISTINCT(avm_outputs.transaction_id)) AS transaction_count",
		"COUNT(DISTINCT(avm_output_addresses.address)) AS address_count",
		"COUNT(DISTINCT(avm_outputs.asset_id)) AS asset_count",
		"COUNT(avm_outputs.id) AS output_count",
	}
	additionalHours = (365 * 24) * time.Hour
)

type ProducerTasker struct {
	initlock                sync.RWMutex
	connections             *services.Connections
	log                     *logging.Log
	plock                   sync.Mutex
	avmOutputsCursor        func(ctx context.Context, sess *dbr.Session, aggregateTs time.Time) (*sql.Rows, error)
	insertAvmAggregate      func(ctx context.Context, sess *dbr.Session, avmAggregate models.AvmAggregateModel) (sql.Result, error)
	updateAvmAggregate      func(ctx context.Context, sess *dbr.Session, avmAggregate models.AvmAggregateModel) (sql.Result, error)
	insertAvmAggregateCount func(ctx context.Context, sess *dbr.Session, avmAggregate models.AvmAggregateCount) (sql.Result, error)
	updateAvmAggregateCount func(ctx context.Context, sess *dbr.Session, avmAggregate models.AvmAggregateCount) (sql.Result, error)
	timeStampProducer       func() time.Time
}

var producerTaskerInstance = ProducerTasker{
	avmOutputsCursor:        AvmOutputsAggregateCursor,
	insertAvmAggregate:      models.InsertAvmAssetAggregation,
	updateAvmAggregate:      models.UpdateAvmAssetAggregation,
	insertAvmAggregateCount: models.InsertAvmAssetAggregationCount,
	updateAvmAggregateCount: models.UpdateAvmAssetAggregationCount,
	timeStampProducer:       time.Now,
}

func initializeProducerTasker(conf cfg.Config, log *logging.Log) error {
	producerTaskerInstance.initlock.Lock()
	defer producerTaskerInstance.initlock.Unlock()

	if producerTaskerInstance.connections != nil {
		return nil
	}

	connections, err := services.NewConnectionsFromConfig(conf.Services)
	if err != nil {
		return err
	}

	producerTaskerInstance.connections = connections
	producerTaskerInstance.log = log
	producerTaskerInstance.Start()
	return nil
}

func (t *ProducerTasker) Start() {
	go initRefreshAggregatesTick(t)
}

func (t *ProducerTasker) RefreshAggregates() error {
	t.plock.Lock()
	defer t.plock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	job := t.connections.Stream().NewJob("producertasker")
	sess := t.connections.DB().NewSession(job)

	var err error
	var liveAggregationState models.AvmAssetAggregateStateModel
	var backupAggregateState models.AvmAssetAggregateStateModel

	// initialize the assset_aggregation_state table with id=stateLiveId row.
	// if the row has not been created..
	// created at and current created at set to time(0), so the first run will re-build aggregates for the entire db.
	_, _ = models.InsertAvmAssetAggregationState(ctx, sess,
		models.AvmAssetAggregateStateModel{
			ID:               params.StateLiveId,
			CreatedAt:        time.Unix(1, 0),
			CurrentCreatedAt: time.Unix(1, 0)},
	)

	liveAggregationState, err = models.SelectAvmAssetAggregationState(ctx, sess, params.StateLiveId)
	// this is really bad, the state live row was not created..  we cannot proceed safely.
	if liveAggregationState.ID != params.StateLiveId {
		t.log.Error("unable to find live state")
		return err
	}

	// check if the backup row exists, if found we crashed from a previous run.
	backupAggregateState, _ = models.SelectAvmAssetAggregationState(ctx, sess, params.StateBackupId)

	if backupAggregateState.ID == uint64(params.StateBackupId) {
		// re-process from backup row..
		liveAggregationState = backupAggregateState
	} else {
		// make a copy of the last created_at, and reset to now + 1 years in the future
		// we are using the db as an atomic swap...
		// current_created_at is set to the newest aggregation timestamp from the message queue.
		// and in the same update we reset created_at to a time in the future.
		// when we get new messages from the queue, they will execute the sql _after_ this update, and set created_at to an earlier date.
		updatedCurrentCreated := t.timeStampProducer().Add(additionalHours)
		_, err = sess.ExecContext(ctx, "update avm_asset_aggregation_state "+
			"set current_created_at=created_at, created_at=? "+
			"where id=?", updatedCurrentCreated, params.StateLiveId)
		if err != nil {
			t.log.Error("atomic swap %s", err.Error())
			return err
		}

		// reload the live state
		liveAggregationState, _ = models.SelectAvmAssetAggregationState(ctx, sess, params.StateLiveId)
		// this is really bad, the state live row was not created..  we cannot proceed safely.
		if liveAggregationState.ID != params.StateLiveId {
			t.log.Error("unable to reload live state")
			return err
		}

		backupAggregateState, _ = t.handleBackupState(ctx, sess, liveAggregationState)
	}

	aggregateTS := computeAndRoundCurrentAggregateTS(liveAggregationState.CurrentCreatedAt)

	aggregateTS, err = t.processAvmOutputs(ctx, sess, aggregateTS)
	if err != nil {
		return err
	}

	err = t.processAvmOutputAddressesCounts(ctx, sess, aggregateTS)
	if err != nil {
		return err
	}

	// everything worked, so we can wipe id=stateBackupId backup row
	// lets make sure our run created this row ..  so check for current_created_at match..
	// if we didn't create the row, the creator would delete it..  (some other producer running this code)
	// if things go really bad, then when the process restarts the row will be re-selected and deleted then..
	_, _ = sess.
		DeleteFrom("avm_asset_aggregation_state").
		Where("id = ? and current_created_at = ?", params.StateBackupId, backupAggregateState.CurrentCreatedAt).
		ExecContext(ctx)

	// delete aggregate data before aggregateDeleteFrame
	_, _ = models.PurgeOldAvmAssetAggregation(ctx, sess, aggregateTS.Add(aggregateDeleteFrame))

	t.log.Info("processed up to %s", aggregateTS.String())

	return nil
}

func (t *ProducerTasker) processAvmOutputs(ctx context.Context, sess *dbr.Session, aggregateTS time.Time) (time.Time, error) {
	var err error
	var rows *sql.Rows
	rows, err = t.avmOutputsCursor(ctx, sess, aggregateTS)
	if err != nil {
		t.log.Error("error query %s", err.Error())
		return time.Time{}, err
	}
	if rows.Err() != nil {
		t.log.Error("error query %s", err.Error())
		return time.Time{}, err
	}

	for ok := rows.Next(); ok; ok = rows.Next() {
		var avmAggregates models.AvmAggregateModel
		err = rows.Scan(&avmAggregates.AggregateTS,
			&avmAggregates.AssetId,
			&avmAggregates.TransactionVolume,
			&avmAggregates.TransactionCount,
			&avmAggregates.AddressCount,
			&avmAggregates.AssetCount,
			&avmAggregates.OutputCount)
		if err != nil {
			t.log.Error("row fetch %s", err.Error())
			return time.Time{}, err
		}

		// aggregateTS would be update to the most recent timestamp we processed...
		// we use it later to prune old aggregates from the db.
		if avmAggregates.AggregateTS.After(aggregateTS) {
			aggregateTS = avmAggregates.AggregateTS
		}

		err = t.replaceAvmAggregate(ctx, sess, avmAggregates)
		if err != nil {
			t.log.Error("replace avm aggregate %s", err.Error())
			return time.Time{}, err
		}
	}
	return aggregateTS, nil
}

func (t *ProducerTasker) processAvmOutputAddressesCounts(ctx context.Context, sess *dbr.Session, aggregateTS time.Time) error {
	var err error
	var rows *sql.Rows

	subquery := sess.Select("avm_output_addresses.address").
		Distinct().
		From("avm_output_addresses").
		Where("avm_output_addresses.created_at >= ?", aggregateTS)

	rows, err = sess.
		Select(
			"avm_output_addresses.address",
			"avm_outputs.asset_id",
			"COUNT(DISTINCT(avm_outputs.transaction_id)) AS transaction_count",
			"CAST(COALESCE(SUM(avm_outputs.amount), 0) AS CHAR) AS total_received",
			"CAST(COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id != '' THEN avm_outputs.amount ELSE 0 END), 0) AS CHAR) AS total_sent",
			"CAST(COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id = '' THEN avm_outputs.amount ELSE 0 END), 0) AS CHAR) AS balance",
			"COALESCE(SUM(CASE WHEN avm_outputs.redeeming_transaction_id = '' THEN 1 ELSE 0 END), 0) AS utxo_count",
		).
		From("avm_outputs").
		LeftJoin("avm_output_addresses", "avm_output_addresses.output_id = avm_outputs.id").
		Where("avm_output_addresses.address IN ?", subquery).
		GroupBy("avm_output_addresses.address", "avm_outputs.asset_id").
		RowsContext(ctx)
	if err != nil {
		t.log.Error("error query %s", err.Error())
		return err
	}
	if rows.Err() != nil {
		t.log.Error("error query %s", err.Error())
		return err
	}

	for ok := rows.Next(); ok; ok = rows.Next() {
		var avmAggregatesCount models.AvmAggregateCount
		err = rows.Scan(&avmAggregatesCount.Address,
			&avmAggregatesCount.AssetID,
			&avmAggregatesCount.TransactionCount,
			&avmAggregatesCount.TotalReceived,
			&avmAggregatesCount.TotalSent,
			&avmAggregatesCount.Balance,
			&avmAggregatesCount.UtxoCount)
		if err != nil {
			t.log.Error("row fetch %s", err.Error())
			return err
		}

		err = t.replaceAvmAggregateCount(ctx, sess, avmAggregatesCount)
		if err != nil {
			t.log.Error("replace avm aggregate count %s", err.Error())
			return err
		}
	}
	return nil
}

func (t *ProducerTasker) handleBackupState(ctx context.Context, sess *dbr.Session, liveAggregationState models.AvmAssetAggregateStateModel) (models.AvmAssetAggregateStateModel, error) {
	// setup the backup as a copy of the live state.
	backupAggregateState := liveAggregationState
	backupAggregateState.ID = params.StateBackupId

	var err error
	// id=stateBackupId backup row - for crash recovery
	_, _ = models.InsertAvmAssetAggregationState(ctx, sess, backupAggregateState)

	// update the backup state to the earliest creation time..
	_, err = sess.ExecContext(ctx, "update avm_asset_aggregation_state "+
		"set current_created_at=? "+
		"where id=? and current_created_at > ?",
		backupAggregateState.CurrentCreatedAt, backupAggregateState.ID, backupAggregateState.CurrentCreatedAt)
	if err != nil {
		_, err = models.InsertAvmAssetAggregationState(ctx, sess, backupAggregateState)
		if err != nil {
			t.log.Error("update backup state %s", err.Error())
		}
	}

	return models.SelectAvmAssetAggregationState(ctx, sess, backupAggregateState.ID)
}

func (t *ProducerTasker) replaceAvmAggregate(ctx context.Context, sess *dbr.Session, avmAggregates models.AvmAggregateModel) error {
	_, err := t.insertAvmAggregate(ctx, sess, avmAggregates)
	if db.ErrIsDuplicateEntryError(err) {
		_, err := t.updateAvmAggregate(ctx, sess, avmAggregates)
		// the update failed.  (could be truncation?)... Punt..
		if err != nil {
			return err
		}
	} else
	// the insert failed, not a duplicate.  (could be truncation?)... Punt..
	if err != nil {
		return err
	}
	return nil
}

func (t *ProducerTasker) replaceAvmAggregateCount(ctx context.Context, sess *dbr.Session, avmAggregates models.AvmAggregateCount) error {
	_, err := t.insertAvmAggregateCount(ctx, sess, avmAggregates)
	if db.ErrIsDuplicateEntryError(err) {
		_, err := t.updateAvmAggregateCount(ctx, sess, avmAggregates)
		// the update failed.  (could be truncation?)... Punt..
		if err != nil {
			return err
		}
	} else
	// the insert failed, not a duplicate.  (could be truncation?)... Punt..
	if err != nil {
		return err
	}
	return nil
}

func computeAndRoundCurrentAggregateTS(aggregateTS time.Time) time.Time {
	// round to the nearest minute..
	roundedAggregateTS := aggregateTS.Round(1 * time.Minute)

	// if we rounded half up, then lets just step back 1 minute to avoid losing anything.
	// better to redo a minute than lose one.
	if roundedAggregateTS.After(aggregateTS) {
		aggregateTS = roundedAggregateTS.Add(-1 * time.Minute)
	} else {
		aggregateTS = roundedAggregateTS
	}

	return aggregateTS
}

func (t *ProducerTasker) ConstAggregateDeleteFrame() time.Duration {
	return aggregateDeleteFrame
}

func AvmOutputsAggregateCursor(ctx context.Context, sess *dbr.Session, aggregateTS time.Time) (*sql.Rows, error) {
	rows, err := sess.
		Select(aggregateColumns...).
		From("avm_outputs").
		LeftJoin("avm_output_addresses", "avm_output_addresses.output_id = avm_outputs.id").
		GroupBy("aggregate_ts", "avm_outputs.asset_id").
		Where("avm_outputs.created_at >= ?", aggregateTS).
		RowsContext(ctx)
	return rows, err
}

func initRefreshAggregatesTick(t *ProducerTasker) {
	timer := time.NewTicker(aggregationTick)
	defer timer.Stop()

	_ = t.RefreshAggregates()
	for range timer.C {
		_ = t.RefreshAggregates()
	}
}
// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"strings"
	"time"

	dmysql "github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/parser/charset"
	timodel "github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tiflow/cdc/contextutil"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/sinkv2/eventsink"
	"github.com/pingcap/tiflow/cdc/sinkv2/metrics"
	"github.com/pingcap/tiflow/cdc/sinkv2/metrics/txn"
	"github.com/pingcap/tiflow/pkg/config"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/retry"
	pmysql "github.com/pingcap/tiflow/pkg/sink/mysql"
	"github.com/pingcap/tiflow/pkg/sqlmodel"
	"github.com/pingcap/tiflow/pkg/util"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const (
	// Max interval for flushing transactions to the downstream.
	maxFlushInterval = 10 * time.Millisecond

	// networkDriftDuration is used to construct a context timeout for database operations.
	networkDriftDuration = 5 * time.Second

	defaultDMLMaxRetry uint64 = 8
)

type mysqlBackend struct {
	workerID    int
	changefeed  string
	db          *sql.DB
	cfg         *pmysql.Config
	dmlMaxRetry uint64

	events []*eventsink.TxnCallbackableEvent
	rows   int

	statistics                    *metrics.Statistics
	metricTxnSinkDMLBatchCommit   prometheus.Observer
	metricTxnSinkDMLBatchCallback prometheus.Observer

	maxAllowedPacket int64
}

// NewMySQLBackends creates a new MySQL sink using schema storage
func NewMySQLBackends(
	ctx context.Context,
	sinkURI *url.URL,
	replicaConfig *config.ReplicaConfig,
	dbConnFactory pmysql.Factory,
	statistics *metrics.Statistics,
) ([]*mysqlBackend, error) {
	changefeedID := contextutil.ChangefeedIDFromCtx(ctx)
	changefeed := fmt.Sprintf("%s.%s", changefeedID.Namespace, changefeedID.ID)

	cfg := pmysql.NewConfig()
	err := cfg.Apply(ctx, changefeedID, sinkURI, replicaConfig)
	if err != nil {
		return nil, err
	}

	dsnStr, err := pmysql.GenerateDSN(ctx, sinkURI, cfg, dbConnFactory)
	if err != nil {
		return nil, err
	}

	db, err := dbConnFactory(ctx, dsnStr)
	if err != nil {
		return nil, err
	}

	cfg.IsTiDB, err = pmysql.CheckIsTiDB(ctx, db)
	if err != nil {
		return nil, err
	}

	cfg.IsWriteSourceExisted, err = pmysql.CheckIfBDRModeIsSupported(ctx, db)
	if err != nil {
		return nil, err
	}

	db.SetMaxIdleConns(cfg.WorkerCount)
	db.SetMaxOpenConns(cfg.WorkerCount)

	var maxAllowedPacket int64
	maxAllowedPacket, err = pmysql.QueryMaxAllowedPacket(ctx, db)
	if err != nil {
		log.Warn("failed to query max_allowed_packet, use default value",
			zap.String("changefeed", changefeed),
			zap.Error(err))
		maxAllowedPacket = int64(variable.DefMaxAllowedPacket)
	}

	backends := make([]*mysqlBackend, 0, cfg.WorkerCount)
	for i := 0; i < cfg.WorkerCount; i++ {
		backends = append(backends, &mysqlBackend{
			workerID:    i,
			changefeed:  changefeed,
			db:          db,
			cfg:         cfg,
			dmlMaxRetry: defaultDMLMaxRetry,
			statistics:  statistics,

			metricTxnSinkDMLBatchCommit:   txn.SinkDMLBatchCommit.WithLabelValues(changefeedID.Namespace, changefeedID.ID),
			metricTxnSinkDMLBatchCallback: txn.SinkDMLBatchCallback.WithLabelValues(changefeedID.Namespace, changefeedID.ID),

			maxAllowedPacket: maxAllowedPacket,
		})
	}

	log.Info("MySQL backends is created",
		zap.String("changefeed", changefeed),
		zap.Int("workerCount", cfg.WorkerCount),
		zap.Bool("forceReplicate", cfg.ForceReplicate),
		zap.Bool("enableOldValue", cfg.EnableOldValue))
	return backends, nil
}

// OnTxnEvent implements interface backend.
// It adds the event to the buffer, and return true if it needs flush immediately.
func (s *mysqlBackend) OnTxnEvent(event *eventsink.TxnCallbackableEvent) (needFlush bool) {
	s.events = append(s.events, event)
	s.rows += len(event.Event.Rows)
	return event.Event.ToWaitFlush() || s.rows >= s.cfg.MaxTxnRow
}

// Flush implements interface backend.
func (s *mysqlBackend) Flush(ctx context.Context) (err error) {
	if s.rows == 0 {
		return
	}

	failpoint.Inject("MySQLSinkExecDMLError", func() {
		// Add a delay to ensure the sink worker with `MySQLSinkHangLongTime`
		// failpoint injected is executed first.
		time.Sleep(time.Second * 2)
		failpoint.Return(errors.Trace(dmysql.ErrInvalidConn))
	})

	for _, event := range s.events {
		s.statistics.ObserveRows(event.Event.Rows...)
	}

	dmls := s.prepareDMLs()
	log.Debug("prepare DMLs", zap.String("changefeed", s.changefeed), zap.Any("rows", s.rows),
		zap.Strings("sqls", dmls.sqls), zap.Any("values", dmls.values))

	start := time.Now()
	if err := s.execDMLWithMaxRetries(ctx, dmls); err != nil {
		if errors.Cause(err) != context.Canceled {
			log.Error("execute DMLs failed", zap.String("changefeed", s.changefeed), zap.Error(err))
		}
		return errors.Trace(err)
	}
	startCallback := time.Now()
	for _, callback := range dmls.callbacks {
		callback()
	}
	s.metricTxnSinkDMLBatchCommit.Observe(startCallback.Sub(start).Seconds())
	s.metricTxnSinkDMLBatchCallback.Observe(time.Since(startCallback).Seconds())

	// Be friently to GC.
	for i := 0; i < len(s.events); i++ {
		s.events[i] = nil
	}
	if cap(s.events) > 1024 {
		s.events = make([]*eventsink.TxnCallbackableEvent, 0)
	}
	s.events = s.events[:0]
	s.rows = 0
	return
}

// Close implements interface backend.
func (s *mysqlBackend) Close() (err error) {
	if s.db != nil {
		err = s.db.Close()
		s.db = nil
	}
	return
}

// MaxFlushInterval implements interface backend.
func (s *mysqlBackend) MaxFlushInterval() time.Duration {
	return maxFlushInterval
}

type preparedDMLs struct {
	startTs         []model.Ts
	sqls            []string
	values          [][]interface{}
	callbacks       []eventsink.CallbackFunc
	rowCount        int
	approximateSize int64
}

// convert2RowChanges is a helper function that convert the row change representation
// of CDC into a general one.
func convert2RowChanges(
	row *model.RowChangedEvent,
	tableInfo *timodel.TableInfo,
	changeType sqlmodel.RowChangeType,
) *sqlmodel.RowChange {
	preValues := make([]interface{}, 0, len(row.PreColumns))
	for _, col := range row.PreColumns {
		if col == nil {
			// will not use this value, just append a dummy value
			preValues = append(preValues, "omitted value")
			continue
		}
		preValues = append(preValues, col.Value)
	}

	postValues := make([]interface{}, 0, len(row.Columns))
	for _, col := range row.Columns {
		if col == nil {
			postValues = append(postValues, "omitted value")
			continue
		}
		postValues = append(postValues, col.Value)
	}

	var res *sqlmodel.RowChange

	switch changeType {
	case sqlmodel.RowChangeInsert:
		res = sqlmodel.NewRowChange(
			row.Table,
			nil,
			nil,
			postValues,
			tableInfo,
			nil, nil)
	case sqlmodel.RowChangeUpdate:
		res = sqlmodel.NewRowChange(
			row.Table,
			nil,
			preValues,
			postValues,
			tableInfo,
			nil, nil)
	case sqlmodel.RowChangeDelete:
		res = sqlmodel.NewRowChange(
			row.Table,
			nil,
			preValues,
			nil,
			tableInfo,
			nil, nil)
	}
	res.SetApproximateDataSize(row.ApproximateDataSize)
	return res
}

func convertBinaryToString(cols []*model.Column) {
	for i, col := range cols {
		if col == nil {
			continue
		}
		if col.Charset != "" && col.Charset != charset.CharsetBin {
			colValBytes, ok := col.Value.([]byte)
			if ok {
				cols[i].Value = string(colValBytes)
			}
		}
	}
}

func (s *mysqlBackend) groupRowsByType(
	event *eventsink.TxnCallbackableEvent,
	tableInfo *timodel.TableInfo,
) (insertRows, updateRows, deleteRows [][]*sqlmodel.RowChange) {
	preAllocateSize := len(event.Event.Rows)
	if preAllocateSize > s.cfg.MaxTxnRow {
		preAllocateSize = s.cfg.MaxTxnRow
	}

	insertRow := make([]*sqlmodel.RowChange, 0, preAllocateSize)
	updateRow := make([]*sqlmodel.RowChange, 0, preAllocateSize)
	deleteRow := make([]*sqlmodel.RowChange, 0, preAllocateSize)

	for _, row := range event.Event.Rows {
		convertBinaryToString(row.Columns)
		convertBinaryToString(row.PreColumns)

		if row.IsInsert() {
			insertRow = append(
				insertRow,
				convert2RowChanges(row, tableInfo, sqlmodel.RowChangeInsert))
			if len(insertRow) >= s.cfg.MaxTxnRow {
				insertRows = append(insertRows, insertRow)
				insertRow = make([]*sqlmodel.RowChange, 0, preAllocateSize)
			}
		}

		if row.IsDelete() {
			deleteRow = append(
				deleteRow,
				convert2RowChanges(row, tableInfo, sqlmodel.RowChangeDelete))
			if len(deleteRow) >= s.cfg.MaxTxnRow {
				deleteRows = append(deleteRows, deleteRow)
				deleteRow = make([]*sqlmodel.RowChange, 0, preAllocateSize)
			}
		}

		if row.IsUpdate() {
			updateRow = append(
				updateRow,
				convert2RowChanges(row, tableInfo, sqlmodel.RowChangeUpdate))
			if len(updateRow) >= s.cfg.MaxMultiUpdateRowCount {
				updateRows = append(updateRows, updateRow)
				updateRow = make([]*sqlmodel.RowChange, 0, preAllocateSize)
			}
		}
	}

	if len(insertRow) > 0 {
		insertRows = append(insertRows, insertRow)
	}
	if len(updateRow) > 0 {
		updateRows = append(updateRows, updateRow)
	}
	if len(deleteRow) > 0 {
		deleteRows = append(deleteRows, deleteRow)
	}

	return
}

func (s *mysqlBackend) batchSingleTxnDmls(
	event *eventsink.TxnCallbackableEvent,
	tableInfo *timodel.TableInfo,
	translateToInsert bool,
) (sqls []string, values [][]interface{}) {
	insertRows, updateRows, deleteRows := s.groupRowsByType(event, tableInfo)

	// handle delete
	if len(deleteRows) > 0 {
		for _, rows := range deleteRows {
			sql, value := sqlmodel.GenDeleteSQL(rows...)
			sqls = append(sqls, sql)
			values = append(values, value)
		}
	}

	// handle update
	if len(updateRows) > 0 {
		if s.cfg.IsTiDB {
			for _, rows := range updateRows {
				s, v := s.genUpdateSQL(rows...)
				sqls = append(sqls, s...)
				values = append(values, v...)
			}
			// The behavior of update statement differs between TiDB and MySQL.
			// So we don't use batch update statement when downstream is MySQL.
			// Ref:https://docs.pingcap.com/tidb/stable/sql-statement-update#mysql-compatibility
		} else {
			for _, rows := range updateRows {
				for _, row := range rows {
					sql, value := row.GenSQL(sqlmodel.DMLUpdate)
					sqls = append(sqls, sql)
					values = append(values, value)
				}
			}
		}
	}

	// handle insert
	if len(insertRows) > 0 {
		for _, rows := range insertRows {
			if translateToInsert {
				sql, value := sqlmodel.GenInsertSQL(sqlmodel.DMLInsert, rows...)
				sqls = append(sqls, sql)
				values = append(values, value)
			} else {
				sql, value := sqlmodel.GenInsertSQL(sqlmodel.DMLReplace, rows...)
				sqls = append(sqls, sql)
				values = append(values, value)
			}
		}
	}

	return
}

func (s *mysqlBackend) genUpdateSQL(rows ...*sqlmodel.RowChange) ([]string, [][]interface{}) {
	size, count := 0, 0
	for _, r := range rows {
		size += int(r.GetApproximateDataSize())
		count++
	}
	if size < s.cfg.MaxMultiUpdateRowSize*count {
		// use multi update in one SQL
		sql, value := sqlmodel.GenUpdateSQLFast(rows...)
		return []string{sql}, [][]interface{}{value}
	}
	// each row has one independent update SQL.
	sqls := make([]string, 0, len(rows))
	values := make([][]interface{}, 0, len(rows))
	for _, row := range rows {
		sql, value := row.GenSQL(sqlmodel.DMLUpdate)
		sqls = append(sqls, sql)
		values = append(values, value)
	}
	return sqls, values
}

func hasHandleKey(cols []*model.Column) bool {
	for _, col := range cols {
		if col == nil {
			continue
		}
		if col.Flag.IsHandleKey() {
			return true
		}
	}
	return false
}

// prepareDMLs converts model.RowChangedEvent list to query string list and args list
func (s *mysqlBackend) prepareDMLs() *preparedDMLs {
	// TODO: use a sync.Pool to reduce allocations.
	startTs := make([]uint64, 0, s.rows)
	sqls := make([]string, 0, s.rows)
	values := make([][]interface{}, 0, s.rows)
	callbacks := make([]eventsink.CallbackFunc, 0, len(s.events))
	replaces := make(map[string][][]interface{})

	// flushes the cached batch replace or insert DMLs,
	// to keep the sequence of DMLs
	flushCacheDMLs := func() {
		if s.cfg.BatchReplaceEnabled && len(replaces) > 0 {
			replaceSqls, replaceValues := reduceReplace(replaces, s.cfg.BatchReplaceSize)
			sqls = append(sqls, replaceSqls...)
			values = append(values, replaceValues...)
			replaces = make(map[string][][]interface{})
		}
	}

	// translateToInsert control the update and insert behavior
	// we only translate into insert when old value is enabled and safe mode is disabled
	translateToInsert := s.cfg.EnableOldValue && !s.cfg.SafeMode

	rowCount := 0
	approximateSize := int64(0)
	for _, event := range s.events {
		if len(event.Event.Rows) == 0 {
			continue
		}
		rowCount += len(event.Event.Rows)

		firstRow := event.Event.Rows[0]
		if len(startTs) == 0 || startTs[len(startTs)-1] != firstRow.StartTs {
			startTs = append(startTs, firstRow.StartTs)
		}

		// A row can be translated in to INSERT, when it was committed after
		// the table it belongs to been replicating by TiCDC, which means it must not be
		// replicated before, and there is no such row in downstream MySQL.
		translateToInsert = translateToInsert && firstRow.CommitTs > firstRow.ReplicatingTs
		log.Debug("translate to insert",
			zap.String("changefeed", s.changefeed),
			zap.Bool("translateToInsert", translateToInsert),
			zap.Uint64("firstRowCommitTs", firstRow.CommitTs),
			zap.Uint64("firstRowReplicatingTs", firstRow.ReplicatingTs),
			zap.Bool("enableOldValue", s.cfg.EnableOldValue),
			zap.Bool("safeMode", s.cfg.SafeMode))

		if event.Callback != nil {
			callbacks = append(callbacks, event.Callback)
		}

		// TODO: find a better threshold
		enableBatchModeThreshold := 1
		// Determine whether to use batch dml feature here.
		if s.cfg.BatchDMLEnable && len(event.Event.Rows) > enableBatchModeThreshold {
			tableColumns := firstRow.Columns
			if firstRow.IsDelete() {
				tableColumns = firstRow.PreColumns
			}
			// only use batch dml when the table has a handle key
			if hasHandleKey(tableColumns) {
				// TODO(dongmen): find a better way to get table info.
				tableInfo := model.BuildTiDBTableInfo(tableColumns, firstRow.IndexColumns)
				flushCacheDMLs()
				sql, value := s.batchSingleTxnDmls(event, tableInfo, translateToInsert)
				sqls = append(sqls, sql...)
				values = append(values, value...)
				approximateSize += int64(len(sql))
				for _, row := range event.Event.Rows {
					approximateSize += row.ApproximateDataSize
				}
				continue
			}
		}

		quoteTable := firstRow.Table.QuoteString()
		for _, row := range event.Event.Rows {
			var query string
			var args []interface{}
			// If the old value is enabled, is not in safe mode and is an update event, then translate to UPDATE.
			// NOTICE: Only update events with the old value feature enabled will have both columns and preColumns.
			if len(row.PreColumns) != 0 && len(row.Columns) != 0 {
				flushCacheDMLs()
				query, args = prepareUpdate(quoteTable, row.PreColumns, row.Columns, s.cfg.ForceReplicate)
				if query != "" {
					sqls = append(sqls, query)
					values = append(values, args)
				}
				approximateSize += int64(len(query)) + row.ApproximateDataSize
				continue
			}

			// Delete Event
			if len(row.PreColumns) != 0 {
				flushCacheDMLs()
				query, args = prepareDelete(quoteTable, row.PreColumns, s.cfg.ForceReplicate)
				if query != "" {
					sqls = append(sqls, query)
					values = append(values, args)
				}
			}

			// Insert Event
			// It will be translated directly into a
			// INSERT(not in safe mode)
			// or REPLACE(in safe mode) SQL.
			if len(row.Columns) != 0 {
				if s.cfg.BatchReplaceEnabled {
					query, args = prepareReplace(quoteTable, row.Columns, false /* appendPlaceHolder */, translateToInsert)
					if query != "" {
						if _, ok := replaces[query]; !ok {
							replaces[query] = make([][]interface{}, 0)
						}
						replaces[query] = append(replaces[query], args)
					}
				} else {
					query, args = prepareReplace(quoteTable, row.Columns, true /* appendPlaceHolder */, translateToInsert)
					if query != "" {
						sqls = append(sqls, query)
						values = append(values, args)
					}
				}
			}

			approximateSize += int64(len(query)) + row.ApproximateDataSize
		}
	}
	flushCacheDMLs()

	if len(callbacks) == 0 {
		callbacks = nil
	}

	return &preparedDMLs{
		startTs:         startTs,
		sqls:            sqls,
		values:          values,
		callbacks:       callbacks,
		rowCount:        rowCount,
		approximateSize: approximateSize,
	}
}

// execute SQLs in the multi statements way.
func (s *mysqlBackend) multiStmtExecute(
	ctx context.Context, dmls *preparedDMLs, tx *sql.Tx, writeTimeout time.Duration,
) error {
	var multiStmtArgs []any
	for _, value := range dmls.values {
		multiStmtArgs = append(multiStmtArgs, value...)
	}
	multiStmtSQL := strings.Join(dmls.sqls, ";")

	log.Debug("exec row", zap.String("changefeed", s.changefeed), zap.Int("workerID", s.workerID),
		zap.String("sql", multiStmtSQL), zap.Any("args", multiStmtArgs))
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	start := time.Now()
	_, execError := tx.ExecContext(ctx, multiStmtSQL, multiStmtArgs...)
	if execError != nil {
		err := logDMLTxnErr(
			wrapMysqlTxnError(execError),
			start, s.changefeed, multiStmtSQL, dmls.rowCount, dmls.startTs)
		if rbErr := tx.Rollback(); rbErr != nil {
			if errors.Cause(rbErr) != context.Canceled {
				log.Warn("failed to rollback txn", zap.String("changefeed", s.changefeed), zap.Error(rbErr))
			}
		}
		return err
	}
	return nil
}

// execute SQLs in each preparedDMLs one by one in the same transaction.
func (s *mysqlBackend) sequenceExecute(
	ctx context.Context, dmls *preparedDMLs, tx *sql.Tx, writeTimeout time.Duration,
) error {
	start := time.Now()
	for i, query := range dmls.sqls {
		args := dmls.values[i]
		log.Debug("exec row", zap.String("changefeed", s.changefeed), zap.Int("workerID", s.workerID),
			zap.String("sql", query), zap.Any("args", args))
		ctx, cancelFunc := context.WithTimeout(ctx, writeTimeout)
		_, execError := tx.ExecContext(ctx, query, args...)
		if execError != nil {
			err := logDMLTxnErr(
				wrapMysqlTxnError(execError),
				start, s.changefeed, query, dmls.rowCount, dmls.startTs)
			if rbErr := tx.Rollback(); rbErr != nil {
				if errors.Cause(rbErr) != context.Canceled {
					log.Warn("failed to rollback txn", zap.String("changefeed", s.changefeed), zap.Error(rbErr))
				}
			}
			cancelFunc()
			return err
		}
		cancelFunc()
	}
	return nil
}

func (s *mysqlBackend) execDMLWithMaxRetries(pctx context.Context, dmls *preparedDMLs) error {
	if len(dmls.sqls) != len(dmls.values) {
		log.Panic("unexpected number of sqls and values",
			zap.String("changefeed", s.changefeed),
			zap.Strings("sqls", dmls.sqls),
			zap.Any("values", dmls.values))
	}

	start := time.Now()
	fallbackToSeqWay := dmls.approximateSize*2 > s.maxAllowedPacket
	return retry.Do(pctx, func() error {
		writeTimeout, _ := time.ParseDuration(s.cfg.WriteTimeout)
		writeTimeout += networkDriftDuration

		failpoint.Inject("MySQLSinkTxnRandomError", func() {
			log.Warn("inject MySQLSinkTxnRandomError")
			err := logDMLTxnErr(errors.Trace(driver.ErrBadConn), start, s.changefeed, "failpoint", 0, nil)
			failpoint.Return(err)
		})
		failpoint.Inject("MySQLSinkHangLongTime", func() { _ = util.Hang(pctx, time.Hour) })
		failpoint.Inject("MySQLDuplicateEntryError", func() {
			log.Warn("inject MySQLDuplicateEntryError")
			err := logDMLTxnErr(cerror.WrapError(cerror.ErrMySQLDuplicateEntry, &dmysql.MySQLError{
				Number:  uint16(mysql.ErrDupEntry),
				Message: "Duplicate entry",
			}), start, s.changefeed, "failpoint", 0, nil)
			failpoint.Return(err)
		})

		err := s.statistics.RecordBatchExecution(func() (int, int64, error) {
			tx, err := s.db.BeginTx(pctx, nil)
			if err != nil {
				return 0, 0, logDMLTxnErr(
					wrapMysqlTxnError(err),
					start, s.changefeed, "BEGIN", dmls.rowCount, dmls.startTs)
			}

			// If interplated SQL size exceeds maxAllowedPacket, mysql driver will
			// fall back to the sequantial way.
			// error can be ErrPrepareMulti, ErrBadConn etc.
			if s.cfg.MultiStmtEnable && !fallbackToSeqWay {
				err = s.multiStmtExecute(pctx, dmls, tx, writeTimeout)
				if err != nil {
					fallbackToSeqWay = true
					return 0, 0, err
				}
			} else {
				err = s.sequenceExecute(pctx, dmls, tx, writeTimeout)
				if err != nil {
					return 0, 0, err
				}
			}

			// we try to set write source for each txn,
			// so we can use it to trace the data source
			if err = s.setWriteSource(pctx, tx); err != nil {
				err := logDMLTxnErr(
					wrapMysqlTxnError(err),
					start, s.changefeed,
					fmt.Sprintf("SET SESSION %s = %d", "tidb_cdc_write_source",
						s.cfg.SourceID),
					dmls.rowCount, dmls.startTs)
				if rbErr := tx.Rollback(); rbErr != nil {
					if errors.Cause(rbErr) != context.Canceled {
						log.Warn("failed to rollback txn", zap.String("changefeed", s.changefeed), zap.Error(rbErr))
					}
				}
				return 0, 0, err
			}

			if err = tx.Commit(); err != nil {
				return 0, 0, logDMLTxnErr(
					wrapMysqlTxnError(err),
					start, s.changefeed, "COMMIT", dmls.rowCount, dmls.startTs)
			}
			return dmls.rowCount, dmls.approximateSize, nil
		})
		if err != nil {
			return errors.Trace(err)
		}
		log.Debug("Exec Rows succeeded",
			zap.String("changefeed", s.changefeed),
			zap.Int("workerID", s.workerID),
			zap.Int("numOfRows", dmls.rowCount))
		return nil
	}, retry.WithBackoffBaseDelay(pmysql.BackoffBaseDelay.Milliseconds()),
		retry.WithBackoffMaxDelay(pmysql.BackoffMaxDelay.Milliseconds()),
		retry.WithMaxTries(s.dmlMaxRetry),
		retry.WithIsRetryableErr(isRetryableDMLError))
}

func wrapMysqlTxnError(err error) error {
	errCode, ok := getSQLErrCode(err)
	if !ok {
		return cerror.WrapError(cerror.ErrMySQLTxnError, err)
	}
	switch errCode {
	case mysql.ErrDupEntry:
		return cerror.WrapError(cerror.ErrMySQLDuplicateEntry, err)
	}
	return cerror.WrapError(cerror.ErrMySQLTxnError, err)
}

func logDMLTxnErr(
	err error, start time.Time, changefeed string,
	query string, count int, startTs []model.Ts,
) error {
	if len(query) > 1024 {
		query = query[:1024]
	}
	if isRetryableDMLError(err) {
		log.Warn("execute DMLs with error, retry later",
			zap.Error(err), zap.Duration("duration", time.Since(start)),
			zap.String("query", query), zap.Int("count", count),
			zap.Uint64s("startTs", startTs),
			zap.String("changefeed", changefeed))
	} else {
		log.Error("execute DMLs with error, can not retry",
			zap.Error(err), zap.Duration("duration", time.Since(start)),
			zap.String("query", query), zap.Int("count", count),
			zap.String("changefeed", changefeed))
	}
	return errors.WithMessage(err, fmt.Sprintf("Failed query info: %s; ", query))
}

func isRetryableDMLError(err error) bool {
	if !cerror.IsRetryableError(err) {
		return false
	}

	errCode, ok := getSQLErrCode(err)
	if !ok {
		return true
	}

	switch errCode {
	// when meet dup entry error, we don't retry and report the error directly to owner to restart the changefeed.
	case mysql.ErrNoSuchTable, mysql.ErrBadDB, mysql.ErrDupEntry:
		return false
	}
	return true
}

func getSQLErrCode(err error) (errors.ErrCode, bool) {
	mysqlErr, ok := errors.Cause(err).(*dmysql.MySQLError)
	if !ok {
		return -1, false
	}

	return errors.ErrCode(mysqlErr.Number), true
}

// Only for testing.
func (s *mysqlBackend) setDMLMaxRetry(maxRetry uint64) {
	s.dmlMaxRetry = maxRetry
}

// setWriteSource sets write source for the transaction.
func (s *mysqlBackend) setWriteSource(ctx context.Context, txn *sql.Tx) error {
	// we only set write source when donwstream is TiDB and write source is existed.
	if !s.cfg.IsWriteSourceExisted {
		return nil
	}
	// downstream is TiDB, set system variables.
	// We should always try to set this variable, and ignore the error if
	// downstream does not support this variable, it is by design.
	query := fmt.Sprintf("SET SESSION %s = %d", "tidb_cdc_write_source", s.cfg.SourceID)
	_, err := txn.ExecContext(ctx, query)
	if err != nil {
		if mysqlErr, ok := errors.Cause(err).(*dmysql.MySQLError); ok &&
			mysqlErr.Number == mysql.ErrUnknownSystemVariable {
			return nil
		}
		return err
	}
	return nil
}

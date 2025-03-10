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

package puller

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	tidbkv "github.com/pingcap/tidb/pkg/kv"
	timodel "github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/parser/mysql"
	"github.com/pingcap/tiflow/cdc/entry"
	"github.com/pingcap/tiflow/cdc/entry/schema"
	"github.com/pingcap/tiflow/cdc/kv"
	"github.com/pingcap/tiflow/cdc/kv/sharedconn"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/processor/tablepb"
	"github.com/pingcap/tiflow/cdc/puller/memorysorter"
	"github.com/pingcap/tiflow/pkg/config"
	cerror "github.com/pingcap/tiflow/pkg/errors"
	"github.com/pingcap/tiflow/pkg/filter"
	"github.com/pingcap/tiflow/pkg/spanz"
	"github.com/pingcap/tiflow/pkg/txnutil"
	"github.com/pingcap/tiflow/pkg/upstream"
	"github.com/pingcap/tiflow/pkg/util"
	"github.com/tikv/client-go/v2/tikv"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	ddlPullerStuckWarnDuration = 30 * time.Second
	// ddl puller should never filter any DDL jobs even if
	// the changefeed is in BDR mode, because the DDL jobs should
	// be filtered before they are sent to the sink
	ddlPullerFilterLoop = false
)

// DDLJobPuller is used to pull ddl job from TiKV.
// It's used by processor and ddlPullerImpl.
type DDLJobPuller interface {
	util.Runnable

	// Output the DDL job entry, it contains the DDL job and the error.
	Output() <-chan *model.DDLJobEntry
}

// Note: All unexported methods of `ddlJobPullerImpl` should
// be called in the same one goroutine.
type ddlJobPullerImpl struct {
	changefeedID model.ChangeFeedID

	multiplexing bool
	puller       struct {
		Puller
	}
	multiplexingPuller struct {
		*MultiplexingPuller
		sortedDDLCh <-chan *model.RawKVEntry
	}

	kvStorage     tidbkv.Storage
	schemaStorage entry.SchemaStorage
	resolvedTs    uint64
	filter        filter.Filter
	// ddlJobsTable is initialized when receive the first concurrent DDL job.
	// It holds the info of table `tidb_ddl_jobs` of upstream TiDB.
	ddlJobsTable *model.TableInfo
	// It holds the column id of `job_meta` in table `tidb_ddl_jobs`.
	jobMetaColumnID int64
	outputCh        chan *model.DDLJobEntry
}

// Run starts the DDLJobPuller.
func (p *ddlJobPullerImpl) Run(ctx context.Context, _ ...chan<- error) error {
	if p.multiplexing {
		return p.runMultiplexing(ctx)
	}
	return p.run(ctx)
}

// WaitForReady implements util.Runnable.
func (p *ddlJobPullerImpl) WaitForReady(_ context.Context) {}

// Close implements util.Runnable.
func (p *ddlJobPullerImpl) Close() {
	if p.multiplexing {
		p.multiplexingPuller.Close()
	}
}

func (p *ddlJobPullerImpl) handleRawKVEntry(ctx context.Context, ddlRawKV *model.RawKVEntry) error {
	if ddlRawKV == nil {
		return nil
	}

	if ddlRawKV.OpType == model.OpTypeResolved {
		// Only nil in unit test case.
		if p.schemaStorage != nil {
			p.schemaStorage.AdvanceResolvedTs(ddlRawKV.CRTs)
		}
		if ddlRawKV.CRTs > p.getResolvedTs() {
			p.setResolvedTs(ddlRawKV.CRTs)
		}
	}

	job, err := p.unmarshalDDL(ddlRawKV)
	if err != nil {
		return errors.Trace(err)
	}

	if job != nil {
		skip, err := p.handleJob(job)
		if err != nil {
			return err
		}
		log.Info("handle ddl job",
			zap.String("namespace", p.changefeedID.Namespace),
			zap.String("changefeed", p.changefeedID.ID),
			zap.String("query", job.Query),
			zap.Stringer("job", job), zap.Bool("skip", skip))
		if skip {
			return nil
		}
	}

	jobEntry := &model.DDLJobEntry{
		Job:    job,
		OpType: ddlRawKV.OpType,
		CRTs:   ddlRawKV.CRTs,
		Err:    err,
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.outputCh <- jobEntry:
	}
	return nil
}

func (p *ddlJobPullerImpl) run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return errors.Trace(p.puller.Run(ctx)) })
	eg.Go(func() error {
		rawDDLCh := memorysorter.SortOutput(ctx, p.changefeedID, p.puller.Output())
		for {
			var ddlRawKV *model.RawKVEntry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ddlRawKV = <-rawDDLCh:
			}
			if err := p.handleRawKVEntry(ctx, ddlRawKV); err != nil {
				return errors.Trace(err)
			}
		}
	})
	return eg.Wait()
}

func (p *ddlJobPullerImpl) runMultiplexing(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error { return p.multiplexingPuller.Run(ctx) })
	eg.Go(func() error {
		for {
			var ddlRawKV *model.RawKVEntry
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ddlRawKV = <-p.multiplexingPuller.sortedDDLCh:
			}
			if err := p.handleRawKVEntry(ctx, ddlRawKV); err != nil {
				return errors.Trace(err)
			}
		}
	})
	return eg.Wait()
}

// Output the DDL job entry, it contains the DDL job and the error.
func (p *ddlJobPullerImpl) Output() <-chan *model.DDLJobEntry {
	return p.outputCh
}

func (p *ddlJobPullerImpl) getResolvedTs() uint64 {
	return atomic.LoadUint64(&p.resolvedTs)
}

func (p *ddlJobPullerImpl) setResolvedTs(ts uint64) {
	atomic.StoreUint64(&p.resolvedTs, ts)
}

func (p *ddlJobPullerImpl) initJobTableMeta() error {
	version, err := p.kvStorage.CurrentVersion(tidbkv.GlobalTxnScope)
	if err != nil {
		return errors.Trace(err)
	}
	snap := kv.GetSnapshotMeta(p.kvStorage, version.Ver)

	dbInfos, err := snap.ListDatabases()
	if err != nil {
		return cerror.WrapError(cerror.ErrMetaListDatabases, err)
	}

	db, err := findDBByName(dbInfos, mysql.SystemDB)
	if err != nil {
		return errors.Trace(err)
	}

	tbls, err := snap.ListTables(db.ID)
	if err != nil {
		return errors.Trace(err)
	}
	tableInfo, err := findTableByName(tbls, "tidb_ddl_job")
	if err != nil {
		return errors.Trace(err)
	}

	col, err := findColumnByName(tableInfo.Columns, "job_meta")
	if err != nil {
		return errors.Trace(err)
	}

	p.ddlJobsTable = model.WrapTableInfo(db.ID, db.Name.L, 0, tableInfo)
	p.jobMetaColumnID = col.ID
	return nil
}

func (p *ddlJobPullerImpl) unmarshalDDL(rawKV *model.RawKVEntry) (*timodel.Job, error) {
	if rawKV.OpType != model.OpTypePut {
		return nil, nil
	}
	if p.ddlJobsTable == nil && !entry.IsLegacyFormatJob(rawKV) {
		err := p.initJobTableMeta()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return entry.ParseDDLJob(p.ddlJobsTable, rawKV, p.jobMetaColumnID)
}

// handleRenameTables gets all the tables that are renamed
// in the DDL job out and filter them one by one,
// if all the tables are filtered, skip it.
func (p *ddlJobPullerImpl) handleRenameTables(job *timodel.Job) (skip bool, err error) {
	var (
		oldSchemaIDs, newSchemaIDs, oldTableIDs []int64
		newTableNames, oldSchemaNames           []*timodel.CIStr
	)

	err = job.DecodeArgs(&oldSchemaIDs, &newSchemaIDs,
		&newTableNames, &oldTableIDs, &oldSchemaNames)
	if err != nil {
		return true, errors.Trace(err)
	}

	var (
		remainOldSchemaIDs, remainNewSchemaIDs, remainOldTableIDs []int64
		remainNewTableNames, remainOldSchemaNames                 []*timodel.CIStr
	)

	multiTableInfos := job.BinlogInfo.MultipleTableInfos
	if len(multiTableInfos) != len(oldSchemaIDs) ||
		len(multiTableInfos) != len(newSchemaIDs) ||
		len(multiTableInfos) != len(newTableNames) ||
		len(multiTableInfos) != len(oldTableIDs) ||
		len(multiTableInfos) != len(oldSchemaNames) {
		return true, cerror.ErrInvalidDDLJob.GenWithStackByArgs(job.ID)
	}

	// we filter subordinate rename table ddl by these principles:
	// 1. old table name matches the filter rule, remain it.
	// 2. old table name does not match and new table name matches the filter rule, return error.
	// 3. old table name and new table name do not match the filter rule, skip it.
	remainTables := make([]*timodel.TableInfo, 0, len(multiTableInfos))
	snap := p.schemaStorage.GetLastSnapshot()
	for i, tableInfo := range multiTableInfos {
		var shouldDiscardOldTable, shouldDiscardNewTable bool
		oldTable, ok := snap.PhysicalTableByID(tableInfo.ID)
		if !ok {
			shouldDiscardOldTable = true
		} else {
			shouldDiscardOldTable = p.filter.ShouldDiscardDDL(job.Type, oldSchemaNames[i].O, oldTable.Name.O)
		}

		newSchemaName, ok := snap.SchemaByID(newSchemaIDs[i])
		if !ok {
			// the new table name does not hit the filter rule, so we should discard the table.
			shouldDiscardNewTable = true
		} else {
			shouldDiscardNewTable = p.filter.ShouldDiscardDDL(job.Type, newSchemaName.Name.O, newTableNames[i].O)
		}

		if shouldDiscardOldTable && shouldDiscardNewTable {
			// skip a rename table ddl only when its old table name and new table name are both filtered.
			log.Info("RenameTables is filtered",
				zap.Int64("tableID", tableInfo.ID),
				zap.String("schema", oldSchemaNames[i].O),
				zap.String("query", job.Query))
			continue
		}
		if shouldDiscardOldTable && !shouldDiscardNewTable {
			// if old table is not in filter rule and its new name is in filter rule, return error.
			return true, cerror.ErrSyncRenameTableFailed.GenWithStackByArgs(tableInfo.ID, job.Query)
		}
		// old table name matches the filter rule, remain it.
		remainTables = append(remainTables, tableInfo)
		remainOldSchemaIDs = append(remainOldSchemaIDs, oldSchemaIDs[i])
		remainNewSchemaIDs = append(remainNewSchemaIDs, newSchemaIDs[i])
		remainOldTableIDs = append(remainOldTableIDs, oldTableIDs[i])
		remainNewTableNames = append(remainNewTableNames, newTableNames[i])
		remainOldSchemaNames = append(remainOldSchemaNames, oldSchemaNames[i])
	}

	if len(remainTables) == 0 {
		return true, nil
	}

	newArgs := make([]json.RawMessage, 5)
	v, err := json.Marshal(remainOldSchemaIDs)
	if err != nil {
		return true, errors.Trace(err)
	}
	newArgs[0] = v
	v, err = json.Marshal(remainNewSchemaIDs)
	if err != nil {
		return true, errors.Trace(err)
	}
	newArgs[1] = v
	v, err = json.Marshal(remainNewTableNames)
	if err != nil {
		return true, errors.Trace(err)
	}
	newArgs[2] = v
	v, err = json.Marshal(remainOldTableIDs)
	if err != nil {
		return true, errors.Trace(err)
	}
	newArgs[3] = v
	v, err = json.Marshal(remainOldSchemaNames)
	if err != nil {
		return true, errors.Trace(err)
	}
	newArgs[4] = v

	newRawArgs, err := json.Marshal(newArgs)
	if err != nil {
		return true, errors.Trace(err)
	}
	job.RawArgs = newRawArgs
	job.BinlogInfo.MultipleTableInfos = remainTables
	return false, nil
}

// handleJob handles the DDL job.
// It split rename tables DDL job and fill the job table name.
func (p *ddlJobPullerImpl) handleJob(job *timodel.Job) (skip bool, err error) {
	// Only nil in test.
	if p.schemaStorage == nil {
		return false, nil
	}

	defer func() {
		if skip && err == nil {
			log.Info("ddl job schema or table does not match, discard it",
				zap.String("namespace", p.changefeedID.Namespace),
				zap.String("changefeed", p.changefeedID.ID),
				zap.String("schema", job.SchemaName),
				zap.String("table", job.TableName),
				zap.String("query", job.Query),
				zap.Stringer("job", job))
		}
		if err != nil {
			log.Warn("handle ddl job failed",
				zap.String("namespace", p.changefeedID.Namespace),
				zap.String("changefeed", p.changefeedID.ID),
				zap.String("schema", job.SchemaName),
				zap.String("table", job.TableName),
				zap.String("query", job.Query),
				zap.Stringer("job", job),
				zap.Error(err))
		}
	}()

	if job.BinlogInfo.FinishedTS <= p.getResolvedTs() ||
		job.BinlogInfo.SchemaVersion == 0 /* means the ddl is ignored in upstream */ {
		log.Info("ddl job finishedTs less than puller resolvedTs,"+
			"discard the ddl job",
			zap.Uint64("jobFinishedTS", job.BinlogInfo.FinishedTS),
			zap.Uint64("pullerResolvedTs", p.getResolvedTs()),
			zap.String("namespace", p.changefeedID.Namespace),
			zap.String("changefeed", p.changefeedID.ID),
			zap.String("schema", job.SchemaName),
			zap.String("table", job.TableName),
			zap.String("query", job.Query),
			zap.String("job", job.String()))
		return true, nil
	}

	snap := p.schemaStorage.GetLastSnapshot()
	if err := snap.FillSchemaName(job); err != nil {
		log.Info("failed to fill schema name for ddl job", zap.Error(err))
		if p.filter.ShouldDiscardDDL(job.Type, job.SchemaName, job.TableName) {
			return true, nil
		}
		return true, errors.Trace(err)
	}

	switch job.Type {
	case timodel.ActionRenameTables:
		skip, err = p.handleRenameTables(job)
		if err != nil {
			return true, errors.Trace(err)
		}
	case timodel.ActionRenameTable:
		log.Info("rename table ddl job",
			zap.Int64("newSchemaID", job.SchemaID),
			zap.String("newSchemaName", job.SchemaName),
			zap.Int64("tableID", job.TableID),
			zap.String("oldTableName", job.BinlogInfo.TableInfo.Name.O),
			zap.String("newTableName", job.TableName),
		)
		oldTable, ok := snap.PhysicalTableByID(job.TableID)
		if !ok {
			// 1. If we can not find the old table, and the new table name is in filter rule, return error.
			discard := p.filter.ShouldDiscardDDL(job.Type, job.SchemaName, job.BinlogInfo.TableInfo.Name.O)
			if !discard {
				return true, cerror.ErrSyncRenameTableFailed.GenWithStackByArgs(job.TableID, job.Query)
			}
			skip = true
		} else {
			log.Info("rename table ddl job",
				zap.String("oldTableName", oldTable.TableName.Table),
				zap.String("oldSchemaName", oldTable.TableName.Schema))
			// since we can find the old table, we must can find the old schema.
			// 2. If we can find the preTableInfo, we filter it by the old table name.
			skipByOldTableName := p.filter.ShouldDiscardDDL(job.Type, oldTable.TableName.Schema, oldTable.TableName.Table)
			skipByNewTableName := p.filter.ShouldDiscardDDL(job.Type, job.SchemaName, job.BinlogInfo.TableInfo.Name.O)
			// 3. If its old table name is not in filter rule, and its new table name in filter rule, return error.
			if skipByOldTableName && !skipByNewTableName {
				return true, cerror.ErrSyncRenameTableFailed.GenWithStackByArgs(job.TableID, job.Query)
			}
			if skipByOldTableName && skipByNewTableName {
				skip = true
				return true, nil
			}
		}
	default:
		// nil means it is a schema ddl job, it's no need to fill the table name.
		if job.BinlogInfo.TableInfo != nil {
			job.TableName = job.BinlogInfo.TableInfo.Name.O
		}
		skip = p.filter.ShouldDiscardDDL(job.Type, job.SchemaName, job.TableName)
	}

	if skip {
		return true, nil
	}

	err = p.schemaStorage.HandleDDLJob(job)
	if err != nil {
		log.Error("handle ddl job failed",
			zap.String("namespace", p.changefeedID.Namespace),
			zap.String("changefeed", p.changefeedID.ID),
			zap.String("query", job.Query),
			zap.String("schema", job.SchemaName),
			zap.String("table", job.BinlogInfo.TableInfo.Name.O),
			zap.String("job", job.String()),
			zap.Error(err))
		return true, errors.Trace(err)
	}

	p.setResolvedTs(job.BinlogInfo.FinishedTS)

	return p.checkIneligibleTableDDL(snap, job)
}

// checkIneligibleTableDDL checks if the table is ineligible before and after the DDL.
//  1. If it is not a table DDL, we shouldn't check it.
//  2. If the table after the DDL is ineligible:
//     a. If the table is not exist before the DDL, we should ignore the DDL.
//     b. If the table is ineligible before the DDL, we should ignore the DDL.
//     c. If the table is eligible before the DDL, we should return an error.
func (p *ddlJobPullerImpl) checkIneligibleTableDDL(snapBefore *schema.Snapshot, job *timodel.Job) (skip bool, err error) {
	if filter.IsSchemaDDL(job.Type) {
		return false, nil
	}

	snapAfter := p.schemaStorage.GetLastSnapshot()

	if job.Type == timodel.ActionCreateTable {
		// For create table, oldTableID is the new table ID.
		isEligibleAfter := !snapAfter.IsIneligibleTableID(job.BinlogInfo.TableInfo.ID)
		if isEligibleAfter {
			return false, nil
		}
	}

	// For create tables, we always apply the DDL here.
	if job.Type == timodel.ActionCreateTables {
		return false, nil
	}

	oldTableID := job.TableID
	newTableID := job.BinlogInfo.TableInfo.ID

	// If the table is eligible after the DDL, we should apply the DDL.
	// No matter its status before the DDL.
	isEligibleAfter := !p.schemaStorage.GetLastSnapshot().IsIneligibleTableID(newTableID)
	if isEligibleAfter {
		return false, nil
	}

	// Steps here means this table is ineligible after the DDL.
	// We need to check if its status before the DDL.

	// 1. If the table is not in the snapshot before the DDL,
	// we should ignore the DDL.
	_, exist := snapBefore.PhysicalTableByID(oldTableID)
	if !exist {
		return true, nil
	}

	// 2. If the table is ineligible before the DDL, we should ignore the DDL.
	isIneligibleBefore := snapBefore.IsIneligibleTableID(oldTableID)
	if isIneligibleBefore {
		log.Warn("Ignore the DDL event of ineligible table",
			zap.String("changefeed", p.changefeedID.ID), zap.Any("ddl", job))
		return true, nil
	}

	// 3. If the table is eligible before the DDL, we should return an error.
	return false, cerror.New(fmt.Sprintf("An eligible table become ineligible after DDL: [%s] "+
		"it is a dangerous operation and may cause data loss. If you want to replicate this ddl safely, "+
		"pelase pause the changefeed and update the `force-replicate=true` "+
		"in the changefeed configuration, "+
		"then resume the changefeed.", job.Query))
}

func findDBByName(dbs []*timodel.DBInfo, name string) (*timodel.DBInfo, error) {
	for _, db := range dbs {
		if db.Name.L == name {
			return db, nil
		}
	}
	return nil, cerror.WrapError(
		cerror.ErrDDLSchemaNotFound,
		errors.Errorf("can't find schema %s", name))
}

func findTableByName(tbls []*timodel.TableInfo, name string) (*timodel.TableInfo, error) {
	for _, t := range tbls {
		if t.Name.L == name {
			return t, nil
		}
	}
	return nil, cerror.WrapError(
		cerror.ErrDDLSchemaNotFound,
		errors.Errorf("can't find table %s", name))
}

func findColumnByName(cols []*timodel.ColumnInfo, name string) (*timodel.ColumnInfo, error) {
	for _, c := range cols {
		if c.Name.L == name {
			return c, nil
		}
	}
	return nil, cerror.WrapError(
		cerror.ErrDDLSchemaNotFound,
		errors.Errorf("can't find column %s", name))
}

// NewDDLJobPuller creates a new NewDDLJobPuller,
// which fetches ddl events starting from checkpointTs.
func NewDDLJobPuller(
	ctx context.Context,
	up *upstream.Upstream,
	checkpointTs uint64,
	cfg *config.ServerConfig,
	changefeed model.ChangeFeedID,
	schemaStorage entry.SchemaStorage,
	filter filter.Filter,
	isOwner bool,
) (DDLJobPuller, error) {
	pdCli := up.PDClient
	regionCache := up.RegionCache
	kvStorage := up.KVStorage
	pdClock := up.PDClock

	if isOwner {
		changefeed.ID += "_owner_ddl_puller"
	} else {
		changefeed.ID += "_processor_ddl_puller"
	}

	spans := spanz.GetAllDDLSpan()
	for i := range spans {
		// NOTE: kv.SharedClient thinks it's better to use different table ids.
		spans[i].TableID = int64(-1) - int64(i)
	}

	jobPuller := &ddlJobPullerImpl{
		changefeedID:  changefeed,
		multiplexing:  cfg.KVClient.EnableMultiplexing,
		schemaStorage: schemaStorage,
		kvStorage:     kvStorage,
		filter:        filter,
		outputCh:      make(chan *model.DDLJobEntry, defaultPullerOutputChanSize),
	}
	if jobPuller.multiplexing {
		mp := &jobPuller.multiplexingPuller

		rawDDLCh := make(chan *model.RawKVEntry, defaultPullerOutputChanSize)
		mp.sortedDDLCh = memorysorter.SortOutput(ctx, changefeed, rawDDLCh)
		grpcPool := sharedconn.NewConnAndClientPool(up.SecurityConfig, kv.GetGlobalGrpcMetrics())

		client := kv.NewSharedClient(
			changefeed, cfg, ddlPullerFilterLoop,
			pdCli, grpcPool, regionCache, pdClock,
			txnutil.NewLockerResolver(kvStorage.(tikv.Storage), changefeed),
		)

		consume := func(ctx context.Context, raw *model.RawKVEntry, _ []tablepb.Span, _ model.ShouldSplitKVEntry) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case rawDDLCh <- raw:
				return nil
			}
		}
		slots, hasher := 1, func(tablepb.Span, int) int { return 0 }
		mp.MultiplexingPuller = NewMultiplexingPuller(changefeed, client, consume, slots, hasher, 1)

		mp.Subscribe(spans, checkpointTs, memorysorter.DDLPullerTableName, func(_ *model.RawKVEntry) bool { return false })
	} else {
		jobPuller.puller.Puller = New(
			ctx, pdCli, up.GrpcPool, regionCache, kvStorage, pdClock,
			checkpointTs, spans, cfg, changefeed, -1, memorysorter.DDLPullerTableName,
			ddlPullerFilterLoop,
		)
	}

	return jobPuller, nil
}

// DDLPuller is the interface for DDL Puller, used by owner only.
type DDLPuller interface {
	// Run runs the DDLPuller
	Run(ctx context.Context) error
	// PopFrontDDL returns and pops the first DDL job in the internal queue
	PopFrontDDL() (uint64, *timodel.Job)
	// ResolvedTs returns the resolved ts of the DDLPuller
	ResolvedTs() uint64
	// Close closes the DDLPuller
	Close()
}

type ddlPullerImpl struct {
	ddlJobPuller DDLJobPuller

	mu             sync.Mutex
	resolvedTS     uint64
	pendingDDLJobs []*timodel.Job
	lastDDLJobID   int64
	cancel         context.CancelFunc

	changefeedID model.ChangeFeedID

	clock                      clock.Clock
	lastResolvedTsAdvancedTime time.Time
}

// NewDDLPuller return a puller for DDL Event
func NewDDLPuller(ctx context.Context,
	replicaConfig *config.ReplicaConfig,
	up *upstream.Upstream,
	startTs uint64,
	changefeed model.ChangeFeedID,
	schemaStorage entry.SchemaStorage,
	filter filter.Filter,
) (DDLPuller, error) {
	var puller DDLJobPuller
	var err error

	// storage can be nil only in the test
	if up.KVStorage != nil {
		puller, err = NewDDLJobPuller(
			ctx, up, startTs, config.GetGlobalServerConfig(),
			changefeed, schemaStorage, filter,
			true, /* isOwner */
		)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	return &ddlPullerImpl{
		ddlJobPuller: puller,
		resolvedTS:   startTs,
		cancel:       func() {},
		clock:        clock.New(),
		changefeedID: changefeed,
	}, nil
}

func (h *ddlPullerImpl) handleDDLJobEntry(jobEntry *model.DDLJobEntry) error {
	if jobEntry.OpType == model.OpTypeResolved {
		if jobEntry.CRTs > atomic.LoadUint64(&h.resolvedTS) {
			h.lastResolvedTsAdvancedTime = h.clock.Now()
			atomic.StoreUint64(&h.resolvedTS, jobEntry.CRTs)
		}
		return nil
	}
	job, err := jobEntry.Job, jobEntry.Err
	if err != nil {
		return errors.Trace(err)
	}
	if job == nil {
		return nil
	}
	log.Info("[ddl] handleDDLJobEntry", zap.String("job", job.String()))
	if job.ID == h.lastDDLJobID {
		log.Warn("ignore duplicated DDL job",
			zap.String("namespace", h.changefeedID.Namespace),
			zap.String("changefeed", h.changefeedID.ID),
			zap.String("query", job.Query),
			zap.Int64("jobID", job.ID),
			zap.Any("job", job))
		return nil
	}
	log.Info("receive new ddl job",
		zap.String("namespace", h.changefeedID.Namespace),
		zap.String("changefeed", h.changefeedID.ID),
		zap.String("query", job.Query),
		zap.Int64("jobID", job.ID),
		zap.Any("job", job))

	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingDDLJobs = append(h.pendingDDLJobs, job)
	h.lastDDLJobID = job.ID
	return nil
}

// Run the ddl puller to receive DDL events
func (h *ddlPullerImpl) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	h.cancel = cancel

	g.Go(func() error { return h.ddlJobPuller.Run(ctx) })

	g.Go(func() error {
		ticker := h.clock.Ticker(ddlPullerStuckWarnDuration)
		defer ticker.Stop()
		h.lastResolvedTsAdvancedTime = h.clock.Now()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				duration := h.clock.Since(h.lastResolvedTsAdvancedTime)
				if duration > ddlPullerStuckWarnDuration {
					log.Warn("ddl puller resolved ts has not advanced",
						zap.String("namespace", h.changefeedID.Namespace),
						zap.String("changefeed", h.changefeedID.ID),
						zap.Duration("duration", duration),
						zap.Uint64("resolvedTs", atomic.LoadUint64(&h.resolvedTS)))
				}
			case e := <-h.ddlJobPuller.Output():
				if err := h.handleDDLJobEntry(e); err != nil {
					return errors.Trace(err)
				}
			}
		}
	})

	log.Info("DDL puller started",
		zap.String("namespace", h.changefeedID.Namespace),
		zap.String("changefeed", h.changefeedID.ID),
		zap.Uint64("resolvedTS", atomic.LoadUint64(&h.resolvedTS)))

	defer func() {
		log.Info("DDL puller stopped",
			zap.String("namespace", h.changefeedID.Namespace),
			zap.String("changefeed", h.changefeedID.ID))
	}()

	return g.Wait()
}

// PopFrontDDL return the first pending DDL job and remove it from the pending list
func (h *ddlPullerImpl) PopFrontDDL() (uint64, *timodel.Job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pendingDDLJobs) == 0 {
		return atomic.LoadUint64(&h.resolvedTS), nil
	}
	job := h.pendingDDLJobs[0]
	h.pendingDDLJobs = h.pendingDDLJobs[1:]
	return job.BinlogInfo.FinishedTS, job
}

// Close the ddl puller, release all resources.
func (h *ddlPullerImpl) Close() {
	h.cancel()
	if h.ddlJobPuller != nil {
		h.ddlJobPuller.Close()
	}
	log.Info("DDL puller closed",
		zap.String("namespace", h.changefeedID.Namespace),
		zap.String("changefeed", h.changefeedID.ID))
}

func (h *ddlPullerImpl) ResolvedTs() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pendingDDLJobs) == 0 {
		return atomic.LoadUint64(&h.resolvedTS)
	}
	job := h.pendingDDLJobs[0]
	return job.BinlogInfo.FinishedTS
}

// Copyright 2019 PingCAP, Inc.
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

package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/pingcap/check"
	"github.com/pingcap/log"
	tidbddl "github.com/pingcap/tidb/pkg/ddl"
	"github.com/pingcap/tidb/pkg/meta/metabuild"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/util/dbutil"
	"github.com/pingcap/tidb/pkg/util/filter"
	"github.com/pingcap/tiflow/dm/config"
	"github.com/pingcap/tiflow/dm/pkg/binlog"
	"github.com/pingcap/tiflow/dm/pkg/conn"
	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/cputil"
	"github.com/pingcap/tiflow/dm/pkg/gtid"
	dlog "github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/retry"
	"github.com/pingcap/tiflow/dm/pkg/schema"
	"github.com/pingcap/tiflow/dm/syncer/dbconn"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

var (
	cpid                 = "test_for_db"
	schemaCreateSQL      = ""
	tableCreateSQL       = ""
	clearCheckPointSQL   = ""
	loadCheckPointSQL    = ""
	flushCheckPointSQL   = ""
	deleteCheckPointSQL  = ""
	deleteSchemaPointSQL = ""
)

var _ = check.Suite(&testCheckpointSuite{})

type testCheckpointSuite struct {
	cfg     *config.SubTaskConfig
	mock    sqlmock.Sqlmock
	tracker *schema.Tracker
}

func (s *testCheckpointSuite) SetUpSuite(c *check.C) {
	s.cfg = &config.SubTaskConfig{
		ServerID:   101,
		MetaSchema: "test",
		Name:       "syncer_checkpoint_ut",
		Flavor:     mysql.MySQLFlavor,
	}

	log.SetLevel(zapcore.ErrorLevel)
	var err error

	s.tracker, err = schema.NewTestTracker(context.Background(), s.cfg.Name, nil, dlog.L())
	c.Assert(err, check.IsNil)
}

func (s *testCheckpointSuite) TestUpTest(c *check.C) {
	s.tracker.Reset()
}

func (s *testCheckpointSuite) prepareCheckPointSQL() {
	schemaCreateSQL = fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS `%s`", s.cfg.MetaSchema)
	tableCreateSQL = fmt.Sprintf("CREATE TABLE IF NOT EXISTS `%s`.`%s` .*", s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name))
	flushCheckPointSQL = fmt.Sprintf("INSERT INTO `%s`.`%s` .* VALUES.* ON DUPLICATE KEY UPDATE .*", s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name))
	clearCheckPointSQL = fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE id = \\?", s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name))
	loadCheckPointSQL = fmt.Sprintf("SELECT .* FROM `%s`.`%s` WHERE id = \\?", s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name))
	deleteCheckPointSQL = fmt.Sprintf("DELETE FROM %s WHERE id = \\? AND cp_schema = \\? AND cp_table = \\?", dbutil.TableName(s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name)))
	deleteSchemaPointSQL = fmt.Sprintf("DELETE FROM %s WHERE id = \\? AND cp_schema = \\?", dbutil.TableName(s.cfg.MetaSchema, cputil.SyncerCheckpoint(s.cfg.Name)))
}

// this test case uses sqlmock to simulate all SQL operations in tests.
func (s *testCheckpointSuite) TestCheckPoint(c *check.C) {
	tctx := tcontext.Background()

	cp := NewRemoteCheckPoint(tctx, s.cfg, nil, cpid)
	defer func() {
		s.mock.ExpectClose()
		cp.Close()
	}()

	var err error
	db, mock, err := sqlmock.New()
	c.Assert(err, check.IsNil)
	s.mock = mock

	s.prepareCheckPointSQL()

	mock.ExpectBegin()
	mock.ExpectExec(schemaCreateSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(tableCreateSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(clearCheckPointSQL).WithArgs(cpid).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	dbConn, err := db.Conn(tcontext.Background().Context())
	c.Assert(err, check.IsNil)
	conn := dbconn.NewDBConn(s.cfg, conn.NewBaseConnForTest(dbConn, &retry.FiniteRetryStrategy{}))
	cp.(*RemoteCheckPoint).dbConn = conn
	err = cp.(*RemoteCheckPoint).prepare(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.Clear(tctx), check.IsNil)

	// test operation for global checkpoint
	s.testGlobalCheckPoint(c, cp)

	// test operation for table checkpoint
	s.testTableCheckPoint(c, cp)
}

func (s *testCheckpointSuite) testGlobalCheckPoint(c *check.C, cp CheckPoint) {
	tctx := tcontext.Background()

	// global checkpoint init to min
	c.Assert(cp.GlobalPoint().Position, check.Equals, binlog.MinPosition)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, binlog.MinPosition)

	// try load, but should load nothing
	s.mock.ExpectQuery(loadCheckPointSQL).WillReturnRows(sqlmock.NewRows(nil))
	err := cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, binlog.MinPosition)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, binlog.MinPosition)

	oldMode := s.cfg.Mode
	oldDir := s.cfg.Dir
	defer func() {
		s.cfg.Mode = oldMode
		s.cfg.Dir = oldDir
	}()

	pos1 := mysql.Position{
		Name: "mysql-bin.000003",
		Pos:  1943,
	}

	s.mock.ExpectQuery(loadCheckPointSQL).WithArgs(cpid).WillReturnRows(sqlmock.NewRows(nil))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	cp.SaveGlobalPoint(binlog.Location{Position: pos1})

	s.mock.ExpectBegin()
	s.mock.ExpectExec("(162)?"+flushCheckPointSQL).WithArgs(cpid, "", "", pos1.Name, pos1.Pos, "", "", 0, "", "null", true).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	// Create a new snapshot, and discard it, then create a new snapshot again.
	cp.Snapshot(true)
	cp.DiscardPendingSnapshots()
	snap := cp.Snapshot(true)
	err = cp.FlushPointsExcept(tctx, snap.id, nil, nil, nil)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	// try load from config
	pos1.Pos = 2044
	s.cfg.Mode = config.ModeIncrement
	s.cfg.Meta = &config.Meta{BinLogName: pos1.Name, BinLogPos: pos1.Pos}
	err = cp.LoadMeta(tctx.Ctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	s.cfg.Mode = oldMode
	s.cfg.Meta = nil

	// test save global point
	pos2 := mysql.Position{
		Name: "mysql-bin.000005",
		Pos:  2052,
	}
	cp.SaveGlobalPoint(binlog.Location{Position: pos2})
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos2)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	// test rollback
	cp.Rollback()
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	// save again
	cp.SaveGlobalPoint(binlog.Location{Position: pos2})
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos2)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	// flush + rollback
	s.mock.ExpectBegin()
	s.mock.ExpectExec("(202)?"+flushCheckPointSQL).WithArgs(cpid, "", "", pos2.Name, pos2.Pos, "", "", 0, "", "null", true).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.FlushPointsExcept(tctx, cp.Snapshot(true).id, nil, nil, nil)
	c.Assert(err, check.IsNil)
	cp.Rollback()
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos2)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos2)

	// try load from DB
	pos3 := pos2
	pos3.Pos = pos2.Pos + 1000 // > pos2 to enable save
	cp.SaveGlobalPoint(binlog.Location{Position: pos3})
	columns := []string{"cp_schema", "cp_table", "binlog_name", "binlog_pos", "binlog_gtid", "exit_safe_binlog_name", "exit_safe_binlog_pos", "exit_safe_binlog_gtid", "table_info", "is_global"}
	s.mock.ExpectQuery(loadCheckPointSQL).WithArgs(cpid).WillReturnRows(sqlmock.NewRows(columns).AddRow("", "", pos2.Name, pos2.Pos, "", "", 0, "", "null", true))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos2)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos2)

	// test save older point
	/*var buf bytes.Buffer
	log.SetOutput(&buf)
	cp.SaveGlobalPoint(pos1)
	c.Assert(cp.GlobalPoint(), check.Equals, pos2)
	c.Assert(cp.FlushedGlobalPoint(), check.Equals, pos2)
	matchStr := fmt.Sprintf(".*try to save %s is older than current pos %s", pos1, pos2)
	matchStr = strings.Replace(strings.Replace(matchStr, ")", "\\)", -1), "(", "\\(", -1)
	c.Assert(strings.TrimSpace(buf.String()), Matches, matchStr)
	log.SetOutput(os.Stdout)*/

	// test clear
	s.mock.ExpectBegin()
	s.mock.ExpectExec(clearCheckPointSQL).WithArgs(cpid).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.Clear(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, binlog.MinPosition)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, binlog.MinPosition)

	s.mock.ExpectQuery(loadCheckPointSQL).WillReturnRows(sqlmock.NewRows(nil))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, binlog.MinPosition)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, binlog.MinPosition)

	// try load from mydumper's output
	dir := c.MkDir()

	filename := filepath.Join(dir, "metadata")
	err = os.WriteFile(filename, []byte(
		fmt.Sprintf("SHOW MASTER STATUS:\n\tLog: %s\n\tPos: %d\n\tGTID:\n\nSHOW SLAVE STATUS:\n\tHost: %s\n\tLog: %s\n\tPos: %d\n\tGTID:\n\n", pos1.Name, pos1.Pos, "slave_host", pos1.Name, pos1.Pos+1000)),
		0o644)
	c.Assert(err, check.IsNil)
	s.cfg.Mode = config.ModeAll
	s.cfg.Dir = dir
	c.Assert(cp.LoadMeta(tctx.Ctx), check.IsNil)

	// should flush because checkpoint hasn't been updated before (cp.globalPointCheckOrSaveTime.IsZero() == true).
	snapshot := cp.Snapshot(true)
	c.Assert(snapshot.id, check.Equals, 4)

	s.mock.ExpectQuery(loadCheckPointSQL).WillReturnRows(sqlmock.NewRows(nil))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)

	s.mock.ExpectBegin()
	s.mock.ExpectExec(clearCheckPointSQL).WithArgs(cpid).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.Clear(tctx)
	c.Assert(err, check.IsNil)

	// check dumpling write exitSafeModeLocation in metadata
	err = os.WriteFile(filename, []byte(
		fmt.Sprintf(`SHOW MASTER STATUS:
	Log: %s
	Pos: %d
	GTID:

SHOW SLAVE STATUS:
	Host: %s
	Log: %s
	Pos: %d
	GTID:

SHOW MASTER STATUS: /* AFTER CONNECTION POOL ESTABLISHED */
	Log: %s
	Pos: %d
	GTID:
`, pos1.Name, pos1.Pos, "slave_host", pos1.Name, pos1.Pos+1000, pos2.Name, pos2.Pos)), 0o644)
	c.Assert(err, check.IsNil)
	c.Assert(cp.LoadMeta(tctx.Ctx), check.IsNil)

	// should flush because exitSafeModeLocation is true
	snapshot = cp.Snapshot(true)
	c.Assert(snapshot, check.NotNil)
	s.mock.ExpectBegin()
	s.mock.ExpectExec("(202)?"+flushCheckPointSQL).WithArgs(cpid, "", "", pos1.Name, pos1.Pos, "", pos2.Name, pos2.Pos, "", "null", true).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.FlushPointsExcept(tctx, snapshot.id, nil, nil, nil)
	c.Assert(err, check.IsNil)
	s.mock.ExpectQuery(loadCheckPointSQL).WillReturnRows(sqlmock.NewRows(nil))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.FlushedGlobalPoint().Position, check.Equals, pos1)
	c.Assert(cp.SafeModeExitPoint().Position, check.Equals, pos2)

	// when use async flush, even exitSafeModeLocation is true we won't flush
	c.Assert(cp.LoadMeta(tctx.Ctx), check.IsNil)
	snapshot = cp.Snapshot(false)
	c.Assert(snapshot, check.IsNil)
}

func (s *testCheckpointSuite) testTableCheckPoint(c *check.C, cp CheckPoint) {
	var (
		tctx  = tcontext.Background()
		table = &filter.Table{
			Schema: "test_db",
			Name:   "test_table",
		}
		schemaName = "test_db"
		tableName  = "test_table"
		pos1       = mysql.Position{
			Name: "mysql-bin.000008",
			Pos:  123,
		}
		pos2 = mysql.Position{
			Name: "mysql-bin.000008",
			Pos:  456,
		}
		err error
	)

	// not exist
	older := cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsFalse)

	// save
	cp.SaveTablePoint(table, binlog.Location{Position: pos2}, nil)
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsTrue)

	// rollback, to min
	cp.Rollback()
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsFalse)

	// save again
	cp.SaveTablePoint(table, binlog.Location{Position: pos2}, nil)
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsTrue)

	// flush + rollback
	s.mock.ExpectBegin()
	s.mock.ExpectExec("(284)?"+flushCheckPointSQL).WithArgs(cpid, table.Schema, table.Name, pos2.Name, pos2.Pos, "", "", 0, "", sqlmock.AnyArg(), false).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.FlushPointsExcept(tctx, cp.Snapshot(true).id, nil, nil, nil)
	c.Assert(err, check.IsNil)
	cp.Rollback()
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsTrue)

	// save
	cp.SaveTablePoint(table, binlog.Location{Position: pos2}, nil)
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsTrue)

	// delete
	s.mock.ExpectBegin()
	s.mock.ExpectExec(deleteCheckPointSQL).WithArgs(cpid, schemaName, tableName).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	c.Assert(cp.DeleteTablePoint(tctx, table), check.IsNil)
	s.mock.ExpectBegin()
	s.mock.ExpectExec(deleteSchemaPointSQL).WithArgs(cpid, schemaName).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	c.Assert(cp.DeleteSchemaPoint(tctx, schemaName), check.IsNil)

	ctx := context.Background()

	// test save with table info and rollback
	c.Assert(s.tracker.CreateSchemaIfNotExists(schemaName), check.IsNil)
	stmt, err := parseSQL("create table " + tableName + " (c int);")
	c.Assert(err, check.IsNil)
	err = s.tracker.Exec(ctx, schemaName, stmt)
	c.Assert(err, check.IsNil)
	ti, err := s.tracker.GetTableInfo(table)
	c.Assert(err, check.IsNil)
	cp.SaveTablePoint(table, binlog.Location{Position: pos1}, ti)
	rcp := cp.(*RemoteCheckPoint)
	c.Assert(rcp.points[schemaName][tableName].TableInfo(), check.NotNil)
	c.Assert(rcp.points[schemaName][tableName].flushedPoint.ti, check.IsNil)

	cp.Rollback()
	rcp = cp.(*RemoteCheckPoint)
	c.Assert(rcp.points[schemaName][tableName].TableInfo(), check.IsNil)
	c.Assert(rcp.points[schemaName][tableName].flushedPoint.ti, check.IsNil)

	// test save, flush and rollback to not nil table info
	cp.SaveTablePoint(table, binlog.Location{Position: pos1}, ti)
	tiBytes, _ := json.Marshal(ti)
	s.mock.ExpectBegin()
	s.mock.ExpectExec(flushCheckPointSQL).WithArgs(cpid, schemaName, tableName, pos1.Name, pos1.Pos, "", "", 0, "", string(tiBytes), false).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	lastGlobalPoint := cp.GlobalPoint()
	lastGlobalPointSavedTime := cp.GlobalPointSaveTime()
	c.Assert(cp.FlushPointsExcept(tctx, cp.Snapshot(true).id, nil, nil, nil), check.IsNil)
	c.Assert(cp.GlobalPoint(), check.Equals, lastGlobalPoint)
	c.Assert(cp.GlobalPointSaveTime(), check.Equals, lastGlobalPointSavedTime)
	stmt, err = parseSQL("alter table " + tableName + " add c2 int;")
	c.Assert(err, check.IsNil)
	err = s.tracker.Exec(ctx, schemaName, stmt)
	c.Assert(err, check.IsNil)
	ti2, err := s.tracker.GetTableInfo(table)
	c.Assert(err, check.IsNil)
	cp.SaveTablePoint(table, binlog.Location{Position: pos2}, ti2)
	cp.Rollback()

	// clear, to min
	s.mock.ExpectBegin()
	s.mock.ExpectExec(clearCheckPointSQL).WithArgs(cpid).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	err = cp.Clear(tctx)
	c.Assert(err, check.IsNil)
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsFalse)

	// test save table point less than global point
	func() {
		defer func() {
			r := recover()
			matchStr := ".*less than global checkpoint.*"
			c.Assert(r, check.Matches, matchStr)
		}()
		cp.SaveGlobalPoint(binlog.Location{Position: pos2})
		cp.SaveTablePoint(table, binlog.Location{Position: pos1}, nil)
	}()

	// flush but except + rollback
	s.mock.ExpectBegin()
	s.mock.ExpectExec("(320)?"+flushCheckPointSQL).WithArgs(cpid, "", "", pos2.Name, pos2.Pos, "", "", 0, "", "null", true).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	lastGlobalPoint = cp.GlobalPoint()
	lastGlobalPointSavedTime = cp.GlobalPointSaveTime()
	err = cp.FlushPointsExcept(tctx, cp.Snapshot(true).id, []*filter.Table{table}, nil, nil)
	fmt.Println(cp.GlobalPoint(), lastGlobalPoint)
	c.Assert(cp.GlobalPoint(), check.Equals, lastGlobalPoint)
	c.Assert(cp.GlobalPointSaveTime(), check.Not(check.Equals), lastGlobalPointSavedTime)
	c.Assert(err, check.IsNil)
	cp.Rollback()
	older = cp.IsOlderThanTablePoint(table, binlog.Location{Position: pos1})
	c.Assert(older, check.IsFalse)

	s.mock.ExpectBegin()
	s.mock.ExpectExec(clearCheckPointSQL).WithArgs(cpid).WillReturnResult(sqlmock.NewResult(0, 1))
	s.mock.ExpectCommit()
	c.Assert(cp.Clear(tctx), check.IsNil)
	// load table point and exitSafe, with enable GTID
	s.cfg.EnableGTID = true
	flavor := mysql.MySQLFlavor
	gSetStr := "03fc0263-28c7-11e7-a653-6c0b84d59f30:123"
	gs, _ := gtid.ParserGTID(flavor, gSetStr)
	columns := []string{"cp_schema", "cp_table", "binlog_name", "binlog_pos", "binlog_gtid", "exit_safe_binlog_name", "exit_safe_binlog_pos", "exit_safe_binlog_gtid", "table_info", "is_global"}
	s.mock.ExpectQuery(loadCheckPointSQL).WithArgs(cpid).WillReturnRows(
		sqlmock.NewRows(columns).AddRow("", "", pos2.Name, pos2.Pos, gs.String(), pos2.Name, pos2.Pos, gs.String(), "null", true).
			AddRow(schemaName, tableName, pos2.Name, pos2.Pos, gs.String(), "", 0, "", tiBytes, false))
	err = cp.Load(tctx)
	c.Assert(err, check.IsNil)
	c.Assert(cp.GlobalPoint(), check.DeepEquals, binlog.NewLocation(pos2, gs))
	rcp = cp.(*RemoteCheckPoint)
	c.Assert(rcp.points[schemaName][tableName].TableInfo(), check.NotNil)
	c.Assert(rcp.points[schemaName][tableName].flushedPoint.ti, check.NotNil)
	c.Assert(*rcp.safeModeExitPoint, check.DeepEquals, binlog.NewLocation(pos2, gs))
}

func TestRemoteCheckPointLoadIntoSchemaTracker(t *testing.T) {
	cfg := genDefaultSubTaskConfig4Test()
	cfg.WorkerCount = 0
	ctx := context.Background()

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	dbConn, err := db.Conn(ctx)
	require.NoError(t, err)
	downstreamTrackConn := dbconn.NewDBConn(cfg, conn.NewBaseConnForTest(dbConn, &retry.FiniteRetryStrategy{}))
	schemaTracker, err := schema.NewTestTracker(ctx, cfg.Name, downstreamTrackConn, dlog.L())
	require.NoError(t, err)
	defer schemaTracker.Close() //nolint

	tbl1 := &filter.Table{Schema: "test", Name: "tbl1"}
	tbl2 := &filter.Table{Schema: "test", Name: "tbl2"}

	// before load
	_, err = schemaTracker.GetTableInfo(tbl1)
	require.Error(t, err)
	_, err = schemaTracker.GetTableInfo(tbl2)
	require.Error(t, err)

	cp := NewRemoteCheckPoint(tcontext.Background(), cfg, nil, "1")
	checkpoint := cp.(*RemoteCheckPoint)

	parser, err := conn.GetParserFromSQLModeStr("")
	require.NoError(t, err)
	createNode, err := parser.ParseOneStmt("create table tbl1(id int)", "", "")
	require.NoError(t, err)
	ti, err := tidbddl.BuildTableInfoFromAST(metabuild.NewContext(), createNode.(*ast.CreateTableStmt))
	require.NoError(t, err)

	tp1 := tablePoint{ti: ti}
	tp2 := tablePoint{}
	checkpoint.points[tbl1.Schema] = make(map[string]*binlogPoint)
	checkpoint.points[tbl1.Schema][tbl1.Name] = &binlogPoint{flushedPoint: tp1}
	checkpoint.points[tbl2.Schema][tbl2.Name] = &binlogPoint{flushedPoint: tp2}

	// after load
	err = checkpoint.LoadIntoSchemaTracker(ctx, schemaTracker)
	require.NoError(t, err)
	tableInfo, err := schemaTracker.GetTableInfo(tbl1)
	require.NoError(t, err)
	require.Len(t, tableInfo.Columns, 1)
	_, err = schemaTracker.GetTableInfo(tbl2)
	require.Error(t, err)

	// test BatchCreateTableWithInfo will not meet kv entry too large error

	// create 100K comment string
	comment := make([]byte, 0, 100000)
	for i := 0; i < 100000; i++ {
		comment = append(comment, 'A')
	}
	ti.Comment = string(comment)

	tp1 = tablePoint{ti: ti}
	amount := 100
	for i := 0; i < amount; i++ {
		tableName := fmt.Sprintf("tbl_%d", i)
		checkpoint.points[tbl1.Schema][tableName] = &binlogPoint{flushedPoint: tp1}
	}
	err = checkpoint.LoadIntoSchemaTracker(ctx, schemaTracker)
	require.NoError(t, err)
}

func TestLastFlushOutdated(t *testing.T) {
	cfg := genDefaultSubTaskConfig4Test()
	cfg.WorkerCount = 0
	cfg.CheckpointFlushInterval = 1

	cp := NewRemoteCheckPoint(tcontext.Background(), cfg, nil, "1")
	checkpoint := cp.(*RemoteCheckPoint)
	checkpoint.globalPointSaveTime = time.Now().Add(-2 * time.Second)

	require.True(t, checkpoint.LastFlushOutdated())
	require.Nil(t, checkpoint.Snapshot(true))
	// though snapshot is nil, checkpoint is not outdated
	require.False(t, checkpoint.LastFlushOutdated())
}

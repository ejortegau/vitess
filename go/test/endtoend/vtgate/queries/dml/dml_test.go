/*
Copyright 2022 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dml

import (
	"fmt"
	"testing"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/test/endtoend/utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUniqueLookupDuplicateEntries should fail if the is duplicate in unique lookup column.
func TestUniqueLookupDuplicateEntries(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial row
	utils.Exec(t, mcmp.VtConn, "insert into s_tbl(id, num) values (1,10)")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert duplicate row
	utils.AssertContainsError(t, mcmp.VtConn, "insert into s_tbl(id, num) values (2,10)", "lookup.Create: target: sks.-80.primary: vttablet: "+
		"Duplicate entry '10' for key 'num_vdx_tbl.PRIMARY'")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert duplicate row in multi-row insert multi shard
	utils.AssertContainsError(t, mcmp.VtConn, "insert into s_tbl(id, num) values (3,20), (4,20),(5,30)",
		"transaction rolled back to reverse changes of partial DML execution: target: sks.80-.primary: vttablet: "+
			"Duplicate entry '20' for key 'num_vdx_tbl.PRIMARY'")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert duplicate row in multi-row insert - lookup single shard
	utils.AssertContainsError(t, mcmp.VtConn, "insert into s_tbl(id, num) values (3,20), (4,20)",
		"transaction rolled back to reverse changes of partial DML execution: lookup.Create: target: sks.80-.primary: vttablet: "+
			"Duplicate entry '20' for key 'num_vdx_tbl.PRIMARY'")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert second row to test with limit update.
	utils.Exec(t, mcmp.VtConn, "insert into s_tbl(id, num) values (10,100)")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)] [INT64(10) INT64(100)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")] [INT64(100) VARCHAR("594764E1A2B2D98E")]]`)

	// update with limit 1 succeed.
	utils.Exec(t, mcmp.VtConn, "update s_tbl set num = 30 order by id limit 1")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(30)] [INT64(10) INT64(100)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(30) VARCHAR("166B40B44ABA4BD6")] [INT64(100) VARCHAR("594764E1A2B2D98E")]]`)

	// update to same value on multiple row should fail.
	utils.AssertContainsError(t, mcmp.VtConn, "update s_tbl set num = 40 limit 2",
		"lookup.Create: transaction rolled back to reverse changes of partial DML execution: target: sks.80-.primary: vttablet: "+
			"rpc error: code = AlreadyExists desc = Duplicate entry '40' for key 'num_vdx_tbl.PRIMARY'")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(30)] [INT64(10) INT64(100)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(30) VARCHAR("166B40B44ABA4BD6")] [INT64(100) VARCHAR("594764E1A2B2D98E")]]`)
}

// TestUniqueLookupDuplicateIgnore tests the insert ignore on lookup table.
func TestUniqueLookupDuplicateIgnore(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial row
	utils.Exec(t, mcmp.VtConn, "insert into s_tbl(id, num) values (1,10)")
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert ignore duplicate row
	qr := utils.Exec(t, mcmp.VtConn, "insert ignore into s_tbl(id, num) values (2,10)")
	assert.EqualValues(t, 0, qr.RowsAffected)
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")]]`)

	// insert duplicate row in multi-row insert - lookup single shard
	// Current behavior does not work as expected—one of the rows should be inserted.
	// The issue occurs because the table has two vindex columns (`num` and `col`), both of which ignore nulls during vindex insertion.
	// In the `INSERT IGNORE` case, after the vindex create API call, a verify call checks if the row exists in the lookup table.
	// - If the row exists, it is inserted into the main table.
	// - If the row does not exist, the main table insertion is skipped.
	// Since the `col` column is null, the row is not inserted into the lookup table, causing the main table insertion to be ignored.
	qr = utils.Exec(t, mcmp.VtConn, "insert ignore into s_tbl(id, num) values (3,20), (4,20)")
	assert.EqualValues(t, 1, qr.RowsAffected)
	utils.AssertMatches(t, mcmp.VtConn, "select id, num from s_tbl order by id", `[[INT64(1) INT64(10)] [INT64(3) INT64(20)]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")] [INT64(20) VARCHAR("4EB190C9A2FA169C")]]`)

	// insert duplicate row in multi-row insert - vindex values are not null
	qr = utils.Exec(t, mcmp.VtConn, "insert ignore into s_tbl(id, num, col) values (3,20, 30), (4,20, 40)")
	assert.EqualValues(t, 0, qr.RowsAffected)
	utils.AssertMatches(t, mcmp.VtConn, "select id, num, col from s_tbl order by id", `[[INT64(1) INT64(10) NULL] [INT64(3) INT64(20) NULL]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select num, hex(keyspace_id) from num_vdx_tbl order by num", `[[INT64(10) VARCHAR("166B40B44ABA4BD6")] [INT64(20) VARCHAR("4EB190C9A2FA169C")]]`)
	utils.AssertMatches(t, mcmp.VtConn, "select col, hex(keyspace_id) from col_vdx_tbl order by col", `[[INT64(30) VARCHAR("4EB190C9A2FA169C")]]`)

}

func TestMultiEqual(t *testing.T) {
	if clusterInstance.HasPartialKeyspaces {
		t.Skip("test uses multiple keyspaces, test framework only supports partial keyspace testing for a single keyspace")
	}
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into user_tbl(id, region_id) values (1,2),(3,4)")

	// multi equal query
	qr := mcmp.Exec("update user_tbl set id = 2 where (id, region_id) in ((1,1), (3,4))")
	assert.EqualValues(t, 1, qr.RowsAffected) // multi equal query

	qr = mcmp.Exec("delete from user_tbl where (id, region_id) in ((1,1), (2,4))")
	assert.EqualValues(t, 1, qr.RowsAffected)
}

// TestMultiTableDelete executed multi-table delete queries
func TestMultiTableDelete(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (4,'c')")

	// multi table delete
	qr := mcmp.Exec(`delete o from order_tbl o join oevent_tbl ev where o.oid = ev.oid and ev.ename = 'a'`)
	assert.EqualValues(t, 2, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("c")]]`)

	qr = mcmp.Exec(`delete o from order_tbl o join oevent_tbl ev where o.cust_no = ev.oid`)
	assert.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("c")]]`)
}

// TestDeleteWithLimit executed delete queries with limit
func TestDeleteWithLimit(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into s_tbl(id, num) values (1,10), (4,20), (5,5), (6,15), (7,17), (8,80)")
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")

	// delete with limit
	qr := mcmp.Exec(`delete from s_tbl order by num, id limit 3`)
	require.EqualValues(t, 3, qr.RowsAffected)

	qr = mcmp.Exec(`delete from order_tbl where region_id = 1 limit 1`)
	require.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select id, num from s_tbl order by id`,
		`[[INT64(4) INT64(20)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`)
	// 2 rows matches but limit is 1, so any one of the row can remain in table.
	mcmp.AssertMatchesAnyNoCompare(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)

	// delete with limit
	qr = mcmp.Exec(`delete from s_tbl where num < 25 limit 2`)
	require.EqualValues(t, 2, qr.RowsAffected)

	qr = mcmp.Exec(`delete from order_tbl limit 5`)
	require.EqualValues(t, 3, qr.RowsAffected)

	// check rows
	// 3 rows matches `num < 20` but limit is 2 so any one of them can remain in the table.
	mcmp.AssertMatches(`select id, num from s_tbl order by id`,
		`[[INT64(8) INT64(80)]]`)
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[]`)

	// remove all rows
	mcmp.Exec(`delete from s_tbl`)
	mcmp.Exec(`delete from order_tbl limit 5`)

	// try with limit again on empty table.
	qr = mcmp.Exec(`delete from s_tbl where num < 20 limit 2`)
	require.EqualValues(t, 0, qr.RowsAffected)

	qr = mcmp.Exec(`delete from order_tbl limit 5`)
	require.EqualValues(t, 0, qr.RowsAffected)

}

// TestUpdateWithLimit executed update queries with limit
func TestUpdateWithLimit(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into s_tbl(id, col) values (1,10), (4,20), (5,5), (6,15), (7,17), (8,80)")
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")

	// update with limit
	qr := mcmp.Exec(`update s_tbl set col = 12 order by col, id limit 1`)
	require.EqualValues(t, 1, qr.RowsAffected)

	qr = mcmp.Exec(`update order_tbl set cust_no = 12 where region_id = 1 limit 1`)
	require.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select id, col from s_tbl order by id`,
		`[[INT64(1) INT64(10)] [INT64(4) INT64(20)] [INT64(5) INT64(12)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`)
	// 2 rows matches but limit is 1, so any one of the row can be modified in the table.
	mcmp.AssertMatchesAnyNoCompare(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(12)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(12)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)

	// update with limit
	qr = mcmp.Exec(`update s_tbl set col = 32 where col > 17 limit 1`)
	require.EqualValues(t, 1, qr.RowsAffected)

	qr = mcmp.Exec(`update order_tbl set cust_no = cust_no + 10  limit 5`)
	require.EqualValues(t, 4, qr.RowsAffected)

	// check rows
	// 2 rows matches `col > 17` but limit is 1 so any one of them will be updated.
	mcmp.AssertMatchesAnyNoCompare(`select id, col from s_tbl order by id`,
		`[[INT64(1) INT64(10)] [INT64(4) INT64(32)] [INT64(5) INT64(12)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`,
		`[[INT64(1) INT64(10)] [INT64(4) INT64(20)] [INT64(5) INT64(12)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(32)]]`)
	mcmp.AssertMatchesAnyNoCompare(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(22)] [INT64(1) INT64(2) INT64(12)] [INT64(2) INT64(3) INT64(15)] [INT64(2) INT64(4) INT64(65)]]`,
		`[[INT64(1) INT64(1) INT64(14)] [INT64(1) INT64(2) INT64(22)] [INT64(2) INT64(3) INT64(15)] [INT64(2) INT64(4) INT64(65)]]`)

	// trying with zero limit.
	qr = mcmp.Exec(`update s_tbl set col = 44 limit 0`)
	require.EqualValues(t, 0, qr.RowsAffected)

	qr = mcmp.Exec(`update order_tbl set oid = 44 limit 0`)
	require.EqualValues(t, 0, qr.RowsAffected)

	// trying with limit with no-matching row.
	qr = mcmp.Exec(`update s_tbl set col = 44 where id > 100 limit 2`)
	require.EqualValues(t, 0, qr.RowsAffected)

	qr = mcmp.Exec(`update order_tbl set oid = 44 where region_id > 100 limit 2`)
	require.EqualValues(t, 0, qr.RowsAffected)

}

// TestMultiTableUpdate executed multi-table update queries
func TestMultiTableUpdate(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (4,'c')")

	// multi table update
	qr := mcmp.Exec(`update order_tbl o join oevent_tbl ev on o.oid = ev.oid set ev.ename = 'a' where ev.oid > 3`)
	assert.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("a")]]`)

	qr = mcmp.Exec(`update order_tbl o, oevent_tbl ev set ev.ename = 'xyz' where o.cust_no = ev.oid`)
	assert.EqualValues(t, 2, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("xyz")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("xyz")]]`)
}

// TestDeleteWithSubquery executed delete queries with subqueries
func TestDeleteWithSubquery(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into s_tbl(id, col) values (1,10), (2,10), (3,10), (4,20), (5,5), (6,15), (7,17), (8,80)")
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")

	// delete with subquery on s_tbl
	qr := mcmp.Exec(`delete from s_tbl where id in (select oid from order_tbl)`)
	require.EqualValues(t, 4, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select id, col from s_tbl order by id`,
		`[[INT64(5) INT64(5)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`)
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)

	// delete with subquery on order_tbl
	qr = mcmp.Exec(`delete from order_tbl where cust_no > (select col from s_tbl where id = 7)`)
	require.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select id, col from s_tbl order by id`,
		`[[INT64(5) INT64(5)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`)
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)]]`)

	// delete with subquery from same table (fails on mysql) - subquery get's merged so fails for vitess
	_, err := mcmp.ExecAllowAndCompareError(`delete from s_tbl where id in (select id from s_tbl)`, utils.CompareOptions{})
	require.ErrorContains(t, err, "You can't specify target table 's_tbl' for update in FROM clause (errno 1093) (sqlstate HY000)")

	// delete with subquery from same table (fails on mysql) - subquery not merged so passes for vitess
	qr = utils.Exec(t, mcmp.VtConn, `delete from order_tbl where region_id in (select cust_no from order_tbl)`)
	require.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	utils.AssertMatches(t, mcmp.VtConn, `select id, col from s_tbl order by id`,
		`[[INT64(5) INT64(5)] [INT64(6) INT64(15)] [INT64(7) INT64(17)] [INT64(8) INT64(80)]]`)
	utils.AssertMatches(t, mcmp.VtConn, `select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)]]`)
}

// TestMultiTargetDelete executed multi-target delete queries
func TestMultiTargetDelete(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (2,'c')")

	// multi table delete
	qr := mcmp.Exec(`delete o, ev from order_tbl o join oevent_tbl ev where o.oid = ev.oid and ev.ename = 'a'`)
	assert.EqualValues(t, 4, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(2) VARCHAR("b")] [INT64(2) VARCHAR("c")]]`)

	qr = mcmp.Exec(`delete o, ev from order_tbl o join oevent_tbl ev where o.cust_no = ev.oid`)
	assert.EqualValues(t, 3, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[]`)
}

// TestMultiTargetDeleteMore executed multi-target delete queries with additional cases
func TestMultiTargetDeleteMore(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// multi table delete on empty table.
	qr := mcmp.Exec(`delete o, ev from order_tbl o join oevent_tbl ev on o.oid = ev.oid`)
	assert.EqualValues(t, 0, qr.RowsAffected)

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (2,'c')")

	// multi table delete on non-existent data.
	qr = mcmp.Exec(`delete o, ev from order_tbl o join oevent_tbl ev on o.oid = ev.oid where ev.oid = 10`)
	assert.EqualValues(t, 0, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(2) VARCHAR("c")] [INT64(3) VARCHAR("a")]]`)

	// multi table delete with rollback
	mcmp.Exec(`begin`)
	qr = mcmp.Exec(`delete o, ev from order_tbl o join oevent_tbl ev on o.oid = ev.oid where o.cust_no != 4`)
	assert.EqualValues(t, 5, qr.RowsAffected)
	mcmp.Exec(`rollback`)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(2) VARCHAR("c")] [INT64(3) VARCHAR("a")]]`)
}

// TestMultiTargetUpdate executed multi-target update queries
func TestMultiTargetUpdate(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (4,'c')")

	// multi target update
	qr := mcmp.Exec(`update order_tbl o join oevent_tbl ev on o.oid = ev.oid set ev.ename = 'a', o.cust_no = 1 where ev.oid > 3`)
	assert.EqualValues(t, 2, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(1)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("a")]]`)

	qr = mcmp.Exec(`update order_tbl o, oevent_tbl ev set ev.ename = 'xyz', o.oid = 40 where o.cust_no = ev.oid and ev.ename = 'b'`)
	assert.EqualValues(t, 2, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid, region_id`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(1)] [INT64(1) INT64(40) INT64(2)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("xyz")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("a")]]`)
}

// TestMultiTargetNonLiteralUpdate executed multi-target update queries with non-literal values.
func TestMultiTargetNonLiteralUpdate(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into order_tbl(region_id, oid, cust_no) values (1,1,4), (1,2,2), (2,3,5), (2,4,55)")
	mcmp.Exec("insert into oevent_tbl(oid, ename) values (1,'a'), (2,'b'), (3,'a'), (4,'c')")

	// multi target update
	qr := mcmp.Exec(`update order_tbl o join oevent_tbl ev on o.oid = ev.oid set ev.ename = o.cust_no where ev.oid > 3`)
	assert.EqualValues(t, 1, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(1) INT64(2) INT64(2)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("b")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("55")]]`)

	qr = mcmp.Exec(`update order_tbl o, oevent_tbl ev set ev.ename = 'xyz', o.oid = ev.oid + 40 where o.cust_no = ev.oid and ev.ename = 'b'`)
	assert.EqualValues(t, 2, qr.RowsAffected)

	// check rows
	mcmp.AssertMatches(`select region_id, oid, cust_no from order_tbl order by oid, region_id`,
		`[[INT64(1) INT64(1) INT64(4)] [INT64(2) INT64(3) INT64(5)] [INT64(2) INT64(4) INT64(55)] [INT64(1) INT64(42) INT64(2)]]`)
	mcmp.AssertMatches(`select oid, ename from oevent_tbl order by oid`,
		`[[INT64(1) VARCHAR("a")] [INT64(2) VARCHAR("xyz")] [INT64(3) VARCHAR("a")] [INT64(4) VARCHAR("55")]]`)
}

// TestDMLInUnique for update/delete statement using an IN clause with the Vindexes,
// the query is correctly split according to the corresponding values in the IN list.
func TestDMLInUnique(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// initial rows
	mcmp.Exec("insert into user_tbl(id, region_id, `name`) values (1,1,'a'),(2,2,'a'),(3,3,'a'),(4,4,'a'),(5,5,'a'),(6,6,'a')")

	qr := mcmp.Exec("update user_tbl set `name` = 'b' where region_id in (1,2,3,4,5,6)")
	assert.EqualValues(t, 6, qr.RowsAffected)
	qr = mcmp.Exec("delete from user_tbl where region_id in (1,2,3,4,5,6)")
	assert.EqualValues(t, 6, qr.RowsAffected)
	mcmp.Exec("insert into user_tbl(id, region_id, `name`) values (1,1,'a'),(2,2,'a'),(3,3,'a'),(4,4,'a'),(5,5,'a'),(6,6,'a')")

	assertVExplainEquals := func(t *testing.T, conn *mysql.Conn, query, expected string) {
		t.Helper()
		qr := utils.Exec(t, conn, query)
		// strip the first column from each row as it is not deterministic in a VExplain query
		for i := range qr.Rows {
			qr.Rows[i] = qr.Rows[i][1:]
		}
		if err := sqltypes.RowsEqualsStr(expected, qr.Rows); err != nil {
			t.Error(err)
		}
	}
	expected := `[
		[VARCHAR("sks") VARCHAR("-80") VARCHAR("begin")] 
		[VARCHAR("sks") VARCHAR("-80") VARCHAR("update user_tbl set ` + "`name`" + ` = 'b' where region_id in (1, 2, 3, 5)")] 
		[VARCHAR("sks") VARCHAR("80-") VARCHAR("begin")] 
		[VARCHAR("sks") VARCHAR("80-") VARCHAR("update user_tbl set ` + "`name`" + ` = 'b' where region_id in (4, 6)")]
    ]`
	assertVExplainEquals(t, mcmp.VtConn, "vexplain /*vt+ EXECUTE_DML_QUERIES */ queries update user_tbl set `name` = 'b' where region_id in (1,2,3,4,5,6)", expected)

	expected = `[
		[VARCHAR("sks") VARCHAR("-80") VARCHAR("begin")] 
		[VARCHAR("sks") VARCHAR("-80") VARCHAR("delete from user_tbl where region_id in (1, 2, 3, 5)")] 
		[VARCHAR("sks") VARCHAR("80-") VARCHAR("begin")] 
		[VARCHAR("sks") VARCHAR("80-") VARCHAR("delete from user_tbl where region_id in (4, 6)")]
    ]`
	assertVExplainEquals(t, mcmp.VtConn, "vexplain /*vt+ EXECUTE_DML_QUERIES */ queries delete from user_tbl where region_id in (1,2,3,4,5,6)", expected)
}

// TestUpdateWithLargeRowsAsInput tests that a query with large input rows should succeed with passthrough DML on.
func TestUpdateWithLargeRowsAsInput(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// Insert initial rows
	for i := 0; i < 100; i++ {
		mcmp.Exec(fmt.Sprintf("insert into t1(id, col) values (%d, %d)", i, i))
		mcmp.Exec(fmt.Sprintf("insert into t2(id, col) values (%d, %d)", i, i))
	}

	queries := []string{
		`update t1 join t2 on t1.col = t2.col set t1.col = 5`,
		`update t1 join t2 on t1.col = t2.col set t1.col = t1.col + 1`,
		`update t1 set col = col + 1`,
	}

	// Should succeed in OLTP mode.
	for _, query := range queries {
		t.Run("oltp-"+query, func(t *testing.T) {
			mcmp.Exec(query)
		})
	}

	// Switch workload to OLAP
	utils.Exec(t, mcmp.VtConn, `set workload = olap`)

	// Should also succeed in OLAP mode.
	for _, query := range queries {
		t.Run("olap-"+query, func(t *testing.T) {
			mcmp.Exec(query)
		})
	}
}

// TestDeleteWithLargeRowsAsInput tests that a query with large input rows should succeed with passthrough DML on.
func TestDeleteWithLargeRowsAsInput(t *testing.T) {
	mcmp, closer := start(t)
	defer closer()

	// Insert initial rows
	for i := 0; i < 100; i++ {
		mcmp.Exec(fmt.Sprintf("insert into t1(id, col) values (%d, %d)", i, i))
		mcmp.Exec(fmt.Sprintf("insert into t2(id, col) values (%d, %d)", i, i))
	}

	// Should succeed in OLTP mode
	_ = mcmp.Exec(`delete t1 from t1 join t2 on t1.col = t2.col where t1.id > 40`)
	// assert.EqualValues(t, 100, qr.RowsAffected)

	// Switch workload to OLAP
	utils.Exec(t, mcmp.VtConn, `set workload = olap`)

	// Should also succeed in OLAP mode
	_ = mcmp.Exec(`delete t1 from t1 join t2 on t1.col = t2.col`)
	// assert.EqualValues(t, 0, qr.RowsAffected) // All rows should have been deleted in the first run
}

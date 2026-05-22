/*
   Copyright 2022 GitHub Inc.
         See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package logic

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"testing"

	test "github.com/openark/golib/tests"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/binlog"
	"github.com/github/gh-ost/go/sql"
)

var spacesRegexp = regexp.MustCompile(`[ \t\n\r]+`)

func normalizeQuery(name string) string {
	name = strings.Replace(name, "`", "", -1)
	name = spacesRegexp.ReplaceAllString(name, " ")
	name = strings.TrimSpace(name)
	return name
}

func TestApplierGenerateSqlModeQuery(t *testing.T) {
	migrationContext := base.NewMigrationContext()
	applier := NewApplier(migrationContext)

	{
		test.S(t).ExpectEquals(
			applier.generateSqlModeQuery(),
			`sql_mode = CONCAT(@@session.sql_mode, ',NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES')`,
		)
	}
	{
		migrationContext.SkipStrictMode = true
		migrationContext.AllowZeroInDate = false
		test.S(t).ExpectEquals(
			applier.generateSqlModeQuery(),
			`sql_mode = CONCAT(@@session.sql_mode, ',NO_AUTO_VALUE_ON_ZERO')`,
		)
	}
	{
		migrationContext.SkipStrictMode = false
		migrationContext.AllowZeroInDate = true
		test.S(t).ExpectEquals(
			applier.generateSqlModeQuery(),
			`sql_mode = REPLACE(REPLACE(CONCAT(@@session.sql_mode, ',NO_AUTO_VALUE_ON_ZERO,STRICT_ALL_TABLES'), 'NO_ZERO_IN_DATE', ''), 'NO_ZERO_DATE', '')`,
		)
	}
	{
		migrationContext.SkipStrictMode = true
		migrationContext.AllowZeroInDate = true
		test.S(t).ExpectEquals(
			applier.generateSqlModeQuery(),
			`sql_mode = REPLACE(REPLACE(CONCAT(@@session.sql_mode, ',NO_AUTO_VALUE_ON_ZERO'), 'NO_ZERO_IN_DATE', ''), 'NO_ZERO_DATE', '')`,
		)
	}
}

func TestApplierUpdateModifiesUniqueKeyColumns(t *testing.T) {
	columns := sql.NewColumnList([]string{"id", "item_id"})
	columnValues := sql.ToColumnValues([]interface{}{123456, 42})

	migrationContext := base.NewMigrationContext()
	migrationContext.OriginalTableColumns = columns
	migrationContext.UniqueKey = &sql.UniqueKey{
		Name:    t.Name(),
		Columns: *columns,
	}

	applier := NewApplier(migrationContext)

	t.Run("unmodified", func(t *testing.T) {
		modifiedColumn, isModified := applier.updateModifiesUniqueKeyColumns(&binlog.BinlogDMLEvent{
			DatabaseName:      "test",
			DML:               binlog.UpdateDML,
			NewColumnValues:   columnValues,
			WhereColumnValues: columnValues,
		})
		test.S(t).ExpectEquals(modifiedColumn, "")
		test.S(t).ExpectFalse(isModified)
	})

	t.Run("modified", func(t *testing.T) {
		modifiedColumn, isModified := applier.updateModifiesUniqueKeyColumns(&binlog.BinlogDMLEvent{
			DatabaseName:      "test",
			DML:               binlog.UpdateDML,
			NewColumnValues:   sql.ToColumnValues([]interface{}{123456, 24}),
			WhereColumnValues: columnValues,
		})
		test.S(t).ExpectEquals(modifiedColumn, "item_id")
		test.S(t).ExpectTrue(isModified)
	})
}

func TestApplierBuildDMLEventQuery(t *testing.T) {
	columns := sql.NewColumnList([]string{"id", "item_id"})
	columnValues := sql.ToColumnValues([]interface{}{123456, 42})

	migrationContext := base.NewMigrationContext()
	migrationContext.OriginalTableName = "test"
	migrationContext.OriginalTableColumns = columns
	migrationContext.SharedColumns = columns
	migrationContext.MappedSharedColumns = columns
	migrationContext.UniqueKey = &sql.UniqueKey{
		Name:    t.Name(),
		Columns: *columns,
	}

	applier := NewApplier(migrationContext)

	t.Run("delete", func(t *testing.T) {
		binlogEvent := &binlog.BinlogDMLEvent{
			DatabaseName:      "test",
			DML:               binlog.DeleteDML,
			WhereColumnValues: columnValues,
		}

		res := applier.buildDMLEventQuery(binlogEvent)
		test.S(t).ExpectEquals(len(res), 1)
		test.S(t).ExpectNil(res[0].err)
		test.S(t).ExpectEquals(strings.TrimSpace(res[0].query), `delete /* gh-ost `+"`test`.`_test_gho`"+` */
		from
			`+"`test`.`_test_gho`"+`
		where
			((`+"`id`"+` = ?) and (`+"`item_id`"+` = ?))`,
		)
		test.S(t).ExpectEquals(len(res[0].args), 2)
		test.S(t).ExpectEquals(res[0].args[0], 123456)
		test.S(t).ExpectEquals(res[0].args[1], 42)
	})

	t.Run("insert", func(t *testing.T) {
		binlogEvent := &binlog.BinlogDMLEvent{
			DatabaseName:    "test",
			DML:             binlog.InsertDML,
			NewColumnValues: columnValues,
		}
		res := applier.buildDMLEventQuery(binlogEvent)
		test.S(t).ExpectEquals(len(res), 1)
		test.S(t).ExpectNil(res[0].err)
		test.S(t).ExpectEquals(strings.TrimSpace(res[0].query),
			`replace /* gh-ost `+"`test`.`_test_gho`"+` */
		into
			`+"`test`.`_test_gho`"+`
			`+"(`id`, `item_id`)"+`
		values
			(?, ?)`)
		test.S(t).ExpectEquals(len(res[0].args), 2)
		test.S(t).ExpectEquals(res[0].args[0], 123456)
		test.S(t).ExpectEquals(res[0].args[1], 42)
	})

	t.Run("update", func(t *testing.T) {
		binlogEvent := &binlog.BinlogDMLEvent{
			DatabaseName:      "test",
			DML:               binlog.UpdateDML,
			NewColumnValues:   columnValues,
			WhereColumnValues: columnValues,
		}
		res := applier.buildDMLEventQuery(binlogEvent)
		test.S(t).ExpectEquals(len(res), 1)
		test.S(t).ExpectNil(res[0].err)
		test.S(t).ExpectEquals(strings.TrimSpace(res[0].query),
			`update /* gh-ost `+"`test`.`_test_gho`"+` */
			`+"`test`.`_test_gho`"+`
		set
			`+"`id`"+`=?, `+"`item_id`"+`=?
		where
			((`+"`id`"+` = ?) and (`+"`item_id`"+` = ?))`)
		test.S(t).ExpectEquals(len(res[0].args), 4)
		test.S(t).ExpectEquals(res[0].args[0], 123456)
		test.S(t).ExpectEquals(res[0].args[1], 42)
		test.S(t).ExpectEquals(res[0].args[2], 123456)
		test.S(t).ExpectEquals(res[0].args[3], 42)
	})
}

func TestApplierInstantDDL(t *testing.T) {
	migrationContext := base.NewMigrationContext()
	migrationContext.DatabaseName = "test"
	migrationContext.OriginalTableName = "mytable"
	migrationContext.AlterStatementOptions = "ADD INDEX (foo)"
	applier := NewApplier(migrationContext)

	t.Run("instantDDLstmt", func(t *testing.T) {
		stmt := applier.generateInstantDDLQuery()
		test.S(t).ExpectEquals(stmt, "ALTER /* gh-ost */ TABLE `test`.`mytable` ADD INDEX (foo), ALGORITHM=INSTANT")
	})
}

func TestGenerateQuery(t *testing.T) {
	migrationContext := base.NewMigrationContext()
	migrationContext.DatabaseName = "test"
	migrationContext.OriginalTableName = "mytable"
	uniqueColumns := sql.NewColumnList([]string{"id", "order_id"})
	migrationContext.UniqueKey = &sql.UniqueKey{
		Name:    "PRIMARY KEY",
		Columns: *uniqueColumns,
	}
	sharedColumns := sql.NewColumnList([]string{"id", "order_id", "name", "age"})
	migrationContext.SharedColumns = sharedColumns

	applier := NewApplier(migrationContext)

	t.Run("generateDeleteQuery1", func(t *testing.T) {
		stmt := applier.generateDeleteQuery([][]string{{"1", "2"}})
		test.S(t).ExpectEquals(stmt, `
		DELETE /* gh-ost `+"`test`.`mytable`"+` */
		FROM `+"`test`.`_mytable_gho`"+` 
		WHERE `+"(`id`,`order_id`)"+` IN ((1, 2))`)
	})
	t.Run("generateDeleteQuery2", func(t *testing.T) {
		stmt := applier.generateDeleteQuery([][]string{{"'1'", "'2'"}})
		test.S(t).ExpectEquals(stmt, `
		DELETE /* gh-ost `+"`test`.`mytable`"+` */
		FROM `+"`test`.`_mytable_gho`"+` 
		WHERE `+"(`id`,`order_id`)"+` IN (('1', '2'))`)
	})
	t.Run("generateDeleteQuery3", func(t *testing.T) {
		stmt := applier.generateDeleteQuery([][]string{{"'1'", "'2'"}, {"1", "23"}})
		test.S(t).ExpectEquals(stmt, `
		DELETE /* gh-ost `+"`test`.`mytable`"+` */
		FROM `+"`test`.`_mytable_gho`"+` 
		WHERE `+"(`id`,`order_id`)"+` IN (('1', '2'), (1, 23))`)
	})
}

func TestGenerateBatchedReplaceQuery(t *testing.T) {
	migrationContext := base.NewMigrationContext()
	migrationContext.DatabaseName = "test"
	migrationContext.OriginalTableName = "mytable"
	uniqueColumns := sql.NewColumnList([]string{"id"})
	migrationContext.UniqueKey = &sql.UniqueKey{
		Name:    "PRIMARY",
		Columns: *uniqueColumns,
	}
	sharedColumns := sql.NewColumnList([]string{"id", "name", "age"})
	mappedSharedColumns := sql.NewColumnList([]string{"id", "name", "age"})
	migrationContext.SharedColumns = sharedColumns
	migrationContext.MappedSharedColumns = mappedSharedColumns

	applier := NewApplier(migrationContext)

	t.Run("single row", func(t *testing.T) {
		query, args := applier.generateBatchedReplaceQuery([][]interface{}{
			{1, "alice", 30},
		})
		expectedQuery := `
		replace /* gh-ost ` + "`test`.`mytable`" + ` */
		into
			` + "`test`.`_mytable_gho`" + `
			(` + "`id`, `name`, `age`" + `)
		values
			(?, ?, ?)`
		test.S(t).ExpectEquals(normalizeQuery(query), normalizeQuery(expectedQuery))
		test.S(t).ExpectEquals(len(args), 3)
		test.S(t).ExpectEquals(args[0], 1)
		test.S(t).ExpectEquals(args[1], "alice")
		test.S(t).ExpectEquals(args[2], 30)
	})

	t.Run("multiple rows", func(t *testing.T) {
		query, args := applier.generateBatchedReplaceQuery([][]interface{}{
			{1, "alice", 30},
			{2, "bob", 25},
		})
		expectedQuery := `
		replace /* gh-ost ` + "`test`.`mytable`" + ` */
		into
			` + "`test`.`_mytable_gho`" + `
			(` + "`id`, `name`, `age`" + `)
		values
			(?, ?, ?), (?, ?, ?)`
		test.S(t).ExpectEquals(normalizeQuery(query), normalizeQuery(expectedQuery))
		test.S(t).ExpectEquals(len(args), 6)
	})

	t.Run("empty", func(t *testing.T) {
		query, args := applier.generateBatchedReplaceQuery([][]interface{}{})
		test.S(t).ExpectEquals(query, "")
		test.S(t).ExpectTrue(args == nil)
	})

	t.Run("special column types", func(t *testing.T) {
		ctx := base.NewMigrationContext()
		ctx.DatabaseName = "test"
		ctx.OriginalTableName = "mytable"
		cols := sql.NewColumnList([]string{"id", "ts", "data"})
		cols.SetConvertDatetimeToTimestamp("ts", "US/Pacific")
		cols.SetColumnType("data", sql.JSONColumnType)
		ctx.SharedColumns = cols
		ctx.MappedSharedColumns = cols
		ctx.UniqueKey = &sql.UniqueKey{
			Name:    "PRIMARY",
			Columns: *sql.NewColumnList([]string{"id"}),
		}
		a := NewApplier(ctx)
		query, args := a.generateBatchedReplaceQuery([][]interface{}{
			{1, "2024-01-01 00:00:00", `{"key":"val"}`},
		})
		test.S(t).ExpectTrue(strings.Contains(query, "convert_tz(?, 'US/Pacific', '+00:00')"))
		test.S(t).ExpectTrue(strings.Contains(query, "convert(? using utf8mb4)"))
		test.S(t).ExpectEquals(len(args), 3)
	})
}

func TestIsIgnoreOverMaxChunkRangeEvent(t *testing.T) {
	migrationContext := base.NewMigrationContext()
	uniqueColumns := sql.NewColumnList([]string{"id", "date"})
	uniqueColumns.SetColumnCompareValueFunc("id", func(a interface{}, b interface{}) (int, error) {
		_a := new(big.Int)
		if _a, _ = _a.SetString(fmt.Sprintf("%+v", a), 10); a == nil {
			return 0, fmt.Errorf("CompareValueFunc err, %+v convert int is nil", a)
		}
		_b := new(big.Int)
		if _b, _ = _b.SetString(fmt.Sprintf("%+v", b), 10); b == nil {
			return 0, fmt.Errorf("CompareValueFunc err, %+v convert int is nil", b)
		}
		return _a.Cmp(_b), nil
	})

	uniqueColumns.SetColumnCompareValueFunc("date", func(a interface{}, b interface{}) (int, error) {
		_a := new(big.Int)
		if _a, _ = _a.SetString(fmt.Sprintf("%+v", a), 10); a == nil {
			return 0, fmt.Errorf("CompareValueFunc err, %+v convert int is nil", a)
		}
		_b := new(big.Int)
		if _b, _ = _b.SetString(fmt.Sprintf("%+v", b), 10); b == nil {
			return 0, fmt.Errorf("CompareValueFunc err, %+v convert int is nil", b)
		}
		return _a.Cmp(_b), nil
	})

	migrationContext.UniqueKey = &sql.UniqueKey{
		Name:    "PRIMARY KEY",
		Columns: *uniqueColumns,
	}
	migrationContext.MigrationRangeMinValues = sql.ToColumnValues([]interface{}{10, 20240110})
	migrationContext.MigrationRangeMaxValues = sql.ToColumnValues([]interface{}{123456, 20240205})
	migrationContext.MigrationIterationRangeMaxValues = sql.ToColumnValues([]interface{}{11111, 20240103})

	applier := NewApplier(migrationContext)

	t.Run("setFalse", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = false
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{1, 20240101})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("lessRangeMaxValue1", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{100, 20240101})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("lessRangeMaxValue2", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{11111, 20240101})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("equalRangeMaxValue", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{11111, 20240103})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("greatRangeMaxValue1", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{11111, 20240104})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectTrue(isIgnore)
	})

	t.Run("greatRangeMaxValue2", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{11112, 20240103})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectTrue(isIgnore)
	})

	t.Run("lessMaxValue1", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{123456, 20240204})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectTrue(isIgnore)
	})

	t.Run("equalMaxValue", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{123456, 20240205})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("greatMaxValue1", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{123456, 20240207})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("greatMaxValue2", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{123457, 20240204})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})

	t.Run("nilIterationRangeMax", func(t *testing.T) {
		migrationContext.IgnoreOverIterationRangeMaxBinlog = true
		migrationContext.MigrationIterationRangeMaxValues = nil
		// When iteration range max is nil (no chunk copied yet), must NOT ignore events
		isIgnore, err := applier.isIgnoreOverMaxChunkRangeEvent([]interface{}{11112, 20240103})
		test.S(t).ExpectNil(err)
		test.S(t).ExpectFalse(isIgnore)
	})
}

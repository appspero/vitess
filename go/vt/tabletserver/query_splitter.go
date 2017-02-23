package tabletserver

import (
	"encoding/binary"
	"fmt"
	"strconv"

	"github.com/youtube/vitess/go/sqltypes"
	querypb "github.com/youtube/vitess/go/vt/proto/query"
	"github.com/youtube/vitess/go/vt/sqlparser"
	"github.com/youtube/vitess/go/vt/tabletserver/engines/schema"
	"github.com/youtube/vitess/go/vt/tabletserver/querytypes"
)

// QuerySplitter splits a BoundQuery into equally sized smaller queries.
// QuerySplits are generated by adding primary key range clauses to the
// original query. Only a limited set of queries are supported, see
// QuerySplitter.validateQuery() for details. Also, the table must have at least
// one primary key and the leading primary key must be numeric, see
// QuerySplitter.splitBoundaries()
type QuerySplitter struct {
	sql           string
	bindVariables map[string]interface{}
	splitCount    int64
	se            *schema.Engine
	sel           *sqlparser.Select
	tableName     sqlparser.TableIdent
	splitColumn   sqlparser.ColIdent
	rowCount      int64
}

const (
	startBindVarName = "_splitquery_start"
	endBindVarName   = "_splitquery_end"
)

// NewQuerySplitter creates a new QuerySplitter. query is the original query
// to split and splitCount is the desired number of splits. splitCount must
// be a positive int, if not it will be set to 1.
func NewQuerySplitter(
	sql string,
	bindVariables map[string]interface{},
	splitColumn string,
	splitCount int64,
	se *schema.Engine) *QuerySplitter {
	if splitCount < 1 {
		splitCount = 1
	}
	return &QuerySplitter{
		sql:           sql,
		bindVariables: bindVariables,
		splitCount:    splitCount,
		se:            se,
		splitColumn:   sqlparser.NewColIdent(splitColumn),
	}
}

// Ensure that the input query is a Select statement that contains no Join,
// GroupBy, OrderBy, Limit or Distinct operations. Also ensure that the
// source table is present in the schema and has at least one primary key.
func (qs *QuerySplitter) validateQuery() error {
	statement, err := sqlparser.Parse(qs.sql)
	if err != nil {
		return err
	}
	var ok bool
	qs.sel, ok = statement.(*sqlparser.Select)
	if !ok {
		return fmt.Errorf("not a select statement")
	}
	if qs.sel.Distinct != "" || qs.sel.GroupBy != nil ||
		qs.sel.Having != nil || len(qs.sel.From) != 1 ||
		qs.sel.OrderBy != nil || qs.sel.Limit != nil ||
		qs.sel.Lock != "" {
		return fmt.Errorf("unsupported query")
	}
	node, ok := qs.sel.From[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return fmt.Errorf("unsupported query")
	}
	qs.tableName = sqlparser.GetTableName(node.Expr)
	if qs.tableName.IsEmpty() {
		return fmt.Errorf("not a simple table expression")
	}
	table := qs.se.GetTable(qs.tableName)
	if table == nil {
		return fmt.Errorf("can't find table in schema")
	}
	if len(table.PKColumns) == 0 {
		return fmt.Errorf("no primary keys")
	}
	if !qs.splitColumn.IsEmpty() {
		for _, index := range table.Indexes {
			for _, column := range index.Columns {
				if qs.splitColumn.Equal(column) {
					return nil
				}
			}
		}
		return fmt.Errorf("split column is not indexed or does not exist in table schema, SplitColumn: %v, Table: %v", qs.splitColumn, table)
	}
	qs.splitColumn = table.GetPKColumn(0).Name
	return nil
}

// split splits the query into multiple queries. validateQuery() must return
// nil error before split() is called.
func (qs *QuerySplitter) split(columnType querypb.Type, pkMinMax *sqltypes.Result) ([]querytypes.QuerySplit, error) {
	boundaries, err := qs.splitBoundaries(columnType, pkMinMax)
	if err != nil {
		return nil, err
	}
	splits := []querytypes.QuerySplit{}
	// No splits, return the original query as a single split
	if len(boundaries) == 0 {
		splits = append(splits, querytypes.QuerySplit{
			Sql:           qs.sql,
			BindVariables: qs.bindVariables,
		})
	} else {
		boundaries = append(boundaries, sqltypes.Value{})
		whereClause := qs.sel.Where
		// Loop through the boundaries and generated modified where clauses
		start := sqltypes.Value{}
		for _, end := range boundaries {
			bindVars := make(map[string]interface{}, len(qs.bindVariables))
			for k, v := range qs.bindVariables {
				bindVars[k] = v
			}
			qs.sel.Where = qs.getWhereClause(whereClause, bindVars, start, end)
			split := &querytypes.QuerySplit{
				Sql:           sqlparser.String(qs.sel),
				BindVariables: bindVars,
				RowCount:      qs.rowCount,
			}
			splits = append(splits, *split)
			start = end
		}
		qs.sel.Where = whereClause // reset where clause
	}
	return splits, err
}

// getWhereClause returns a whereClause based on desired upper and lower
// bounds for primary key.
func (qs *QuerySplitter) getWhereClause(whereClause *sqlparser.Where, bindVars map[string]interface{}, start, end sqltypes.Value) *sqlparser.Where {
	var startClause *sqlparser.ComparisonExpr
	var endClause *sqlparser.ComparisonExpr
	var clauses sqlparser.Expr
	// No upper or lower bound, just return the where clause of original query
	if start.IsNull() && end.IsNull() {
		return whereClause
	}
	pk := &sqlparser.ColName{
		Name: qs.splitColumn,
	}
	if !start.IsNull() {
		startClause = &sqlparser.ComparisonExpr{
			Operator: sqlparser.GreaterEqualStr,
			Left:     pk,
			Right:    sqlparser.NewValArg([]byte(":" + startBindVarName)),
		}
		bindVars[startBindVarName] = start.ToNative()
	}
	// splitColumn < end
	if !end.IsNull() {
		endClause = &sqlparser.ComparisonExpr{
			Operator: sqlparser.LessThanStr,
			Left:     pk,
			Right:    sqlparser.NewValArg([]byte(":" + endBindVarName)),
		}
		bindVars[endBindVarName] = end.ToNative()
	}
	if startClause == nil {
		clauses = endClause
	} else {
		if endClause == nil {
			clauses = startClause
		} else {
			// splitColumn >= start AND splitColumn < end
			clauses = &sqlparser.AndExpr{
				Left:  startClause,
				Right: endClause,
			}
		}
	}
	if whereClause != nil {
		clauses = &sqlparser.AndExpr{
			Left:  &sqlparser.ParenExpr{Expr: whereClause.Expr},
			Right: &sqlparser.ParenExpr{Expr: clauses},
		}
	}
	return &sqlparser.Where{
		Type: sqlparser.WhereStr,
		Expr: clauses,
	}
}

func (qs *QuerySplitter) splitBoundaries(columnType querypb.Type, pkMinMax *sqltypes.Result) ([]sqltypes.Value, error) {
	switch {
	case sqltypes.IsSigned(columnType):
		return qs.splitBoundariesIntColumn(pkMinMax)
	case sqltypes.IsUnsigned(columnType):
		return qs.splitBoundariesUintColumn(pkMinMax)
	case sqltypes.IsFloat(columnType):
		return qs.splitBoundariesFloatColumn(pkMinMax)
	case sqltypes.IsBinary(columnType):
		return qs.splitBoundariesStringColumn()
	}
	return []sqltypes.Value{}, nil
}

func (qs *QuerySplitter) splitBoundariesIntColumn(pkMinMax *sqltypes.Result) ([]sqltypes.Value, error) {
	boundaries := []sqltypes.Value{}
	if pkMinMax == nil || len(pkMinMax.Rows) != 1 || pkMinMax.Rows[0][0].IsNull() || pkMinMax.Rows[0][1].IsNull() {
		return boundaries, nil
	}
	minNumeric := pkMinMax.Rows[0][0]
	maxNumeric := pkMinMax.Rows[0][1]
	min, err := minNumeric.ParseInt64()
	if err != nil {
		return nil, err
	}
	max, err := maxNumeric.ParseInt64()
	if err != nil {
		return nil, err
	}
	interval := (max - min) / qs.splitCount
	if interval == 0 {
		return nil, err
	}
	qs.rowCount = interval
	for i := int64(1); i < qs.splitCount; i++ {
		v, err := sqltypes.BuildValue(min + interval*i)
		if err != nil {
			return nil, err
		}
		boundaries = append(boundaries, v)
	}
	return boundaries, nil
}

func (qs *QuerySplitter) splitBoundariesUintColumn(pkMinMax *sqltypes.Result) ([]sqltypes.Value, error) {
	boundaries := []sqltypes.Value{}
	if pkMinMax == nil || len(pkMinMax.Rows) != 1 || pkMinMax.Rows[0][0].IsNull() || pkMinMax.Rows[0][1].IsNull() {
		return boundaries, nil
	}
	minNumeric := pkMinMax.Rows[0][0]
	maxNumeric := pkMinMax.Rows[0][1]
	min, err := minNumeric.ParseUint64()
	if err != nil {
		return nil, err
	}
	max, err := maxNumeric.ParseUint64()
	if err != nil {
		return nil, err
	}
	interval := (max - min) / uint64(qs.splitCount)
	if interval == 0 {
		return nil, err
	}
	qs.rowCount = int64(interval)
	for i := uint64(1); i < uint64(qs.splitCount); i++ {
		v, err := sqltypes.BuildValue(min + interval*i)
		if err != nil {
			return nil, err
		}
		boundaries = append(boundaries, v)
	}
	return boundaries, nil
}

func (qs *QuerySplitter) splitBoundariesFloatColumn(pkMinMax *sqltypes.Result) ([]sqltypes.Value, error) {
	boundaries := []sqltypes.Value{}
	if pkMinMax == nil || len(pkMinMax.Rows) != 1 || pkMinMax.Rows[0][0].IsNull() || pkMinMax.Rows[0][1].IsNull() {
		return boundaries, nil
	}
	min, err := strconv.ParseFloat(pkMinMax.Rows[0][0].String(), 64)
	if err != nil {
		return nil, err
	}
	max, err := strconv.ParseFloat(pkMinMax.Rows[0][1].String(), 64)
	if err != nil {
		return nil, err
	}
	interval := (max - min) / float64(qs.splitCount)
	if interval == 0 {
		return nil, err
	}
	qs.rowCount = int64(interval)
	for i := 1; i < int(qs.splitCount); i++ {
		boundary := min + interval*float64(i)
		v, err := sqltypes.BuildValue(boundary)
		if err != nil {
			return nil, err
		}
		boundaries = append(boundaries, v)
	}
	return boundaries, nil
}

// TODO(shengzhe): support split based on min, max from the string column.
func (qs *QuerySplitter) splitBoundariesStringColumn() ([]sqltypes.Value, error) {
	splitRange := int64(0xFFFFFFFF) + 1
	splitSize := splitRange / int64(qs.splitCount)
	//TODO(shengzhe): have a better estimated row count based on table size.
	qs.rowCount = int64(splitSize)
	var boundaries []sqltypes.Value
	for i := 1; i < int(qs.splitCount); i++ {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, uint32(splitSize)*uint32(i))
		val, err := sqltypes.BuildValue(buf)
		if err != nil {
			return nil, err
		}
		boundaries = append(boundaries, val)
	}
	return boundaries, nil
}

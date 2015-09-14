// Copyright 2013 The ql Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSES/QL-LICENSE file.

// Copyright 2015 PingCAP, Inc.
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

package stmts

import (
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/column"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/expressions"
	"github.com/pingcap/tidb/field"
	mysql "github.com/pingcap/tidb/mysqldef"
	"github.com/pingcap/tidb/plan"
	"github.com/pingcap/tidb/plan/plans"
	"github.com/pingcap/tidb/rset"
	"github.com/pingcap/tidb/rset/rsets"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/stmt"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/format"
	"github.com/pingcap/tidb/util/types"
)

var _ stmt.Statement = (*UpdateStmt)(nil)

// UpdateStmt is a statement to update columns of existing rows in tables with new values.
// See: https://dev.mysql.com/doc/refman/5.7/en/update.html
type UpdateStmt struct {
	TableRefs     *rsets.JoinRset
	List          []expressions.Assignment
	Where         expression.Expression
	Order         *rsets.OrderByRset
	Limit         *rsets.LimitRset
	LowPriority   bool
	Ignore        bool
	MultipleTable bool

	Text string
}

// Explain implements the stmt.Statement Explain interface.
func (s *UpdateStmt) Explain(ctx context.Context, w format.Formatter) {
	p, err := s.plan(ctx)
	if err != nil {
		log.Error(err)
		return
	}
	if p != nil {
		p.Explain(w)
	}
	w.Format("└Update fields %v\n", s.List)
}

// IsDDL implements the stmt.Statement IsDDL interface.
func (s *UpdateStmt) IsDDL() bool {
	return false
}

// OriginText implements the stmt.Statement OriginText interface.
func (s *UpdateStmt) OriginText() string {
	return s.Text
}

// SetText implements the stmt.Statement SetText interface.
func (s *UpdateStmt) SetText(text string) {
	s.Text = text
}

func getUpdateColumns(t table.Table, assignList []expressions.Assignment, multipleTable bool) ([]*column.Col, error) {
	tcols := make([]*column.Col, 0, len(assignList))
	tname := t.TableName()
	for _, asgn := range assignList {
		if multipleTable {
			if !strings.EqualFold(tname.O, asgn.TableName) {
				continue
			}
		}
		col := column.FindCol(t.Cols(), asgn.ColName)
		if col == nil {
			if multipleTable {
				continue
			}
			return nil, errors.Errorf("UPDATE: unknown column %s", asgn.ColName)
		}
		tcols = append(tcols, col)
	}
	return tcols, nil
}

func getInsertValue(name string, cols []*column.Col, row []interface{}) (interface{}, error) {
	for i, col := range cols {
		if col.Name.L == name {
			return row[i], nil
		}
	}
	return nil, errors.Errorf("unknown field %s", name)
}

func getIdentValue(name string, fields []*field.ResultField, row []interface{}, flag uint32) (interface{}, error) {
	indices := field.GetResultFieldIndex(name, fields, flag)
	if len(indices) == 0 {
		return nil, errors.Errorf("unknown field %s", name)
	}
	index := indices[0]
	return row[index], nil
}

func updateRecord(ctx context.Context, h int64, data []interface{}, t table.Table, tcols []*column.Col, assignList []expressions.Assignment, insertData []interface{}, args map[interface{}]interface{}) error {
	if err := t.LockRow(ctx, h, true); err != nil {
		return errors.Trace(err)
	}

	oldData := make([]interface{}, len(t.Cols()))
	touched := make([]bool, len(t.Cols()))
	copy(oldData, data)

	// Generate new values
	m := args
	if m == nil {
		m = make(map[interface{}]interface{}, len(t.Cols()))
		// Set parameter for evaluating expression.
		for _, col := range t.Cols() {
			m[col.Name.L] = data[col.Offset]
		}
	}
	if insertData != nil {
		m[expressions.ExprEvalValuesFunc] = func(name string) (interface{}, error) {
			return getInsertValue(name, t.Cols(), insertData)
		}
	}

	for i, asgn := range assignList {
		val, err := asgn.Expr.Eval(ctx, m)
		if err != nil {
			return err
		}
		colIndex := tcols[i].Offset
		touched[colIndex] = true
		data[colIndex] = val
	}

	// Check whether new value is valid.
	if err := column.CastValues(ctx, data, t.Cols()); err != nil {
		return err
	}

	if err := column.CheckNotNull(t.Cols(), data); err != nil {
		return err
	}

	// If row is not changed, we should do nothing.
	rowChanged := false
	for i, d := range data {
		if !touched[i] {
			continue
		}
		od := oldData[i]
		n, err := types.Compare(d, od)
		if err != nil {
			return errors.Trace(err)
		}

		if n != 0 {
			rowChanged = true
			break
		}
	}
	if !rowChanged {
		// See: https://dev.mysql.com/doc/refman/5.7/en/mysql-real-connect.html  CLIENT_FOUND_ROWS
		if variable.GetSessionVars(ctx).ClientCapability&mysql.ClientFoundRows > 0 {
			variable.GetSessionVars(ctx).AddAffectedRows(1)
		}
		return nil
	}

	// Update record to new value and update index.
	err := t.UpdateRecord(ctx, h, oldData, data, touched)
	if err != nil {
		return errors.Trace(err)
	}
	// Record affected rows.
	if len(insertData) == 0 {
		variable.GetSessionVars(ctx).AddAffectedRows(1)
	} else {
		variable.GetSessionVars(ctx).AddAffectedRows(2)

	}
	return nil
}

func (s *UpdateStmt) plan(ctx context.Context) (plan.Plan, error) {
	var (
		r   plan.Plan
		err error
	)
	if s.TableRefs != nil {
		r, err = s.TableRefs.Plan(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if s.Where != nil {
		r, err = (&rsets.WhereRset{Expr: s.Where, Src: r}).Plan(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if s.Order != nil {
		s.Order.Src = r
		r, err = s.Order.Plan(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	if s.Limit != nil {
		s.Limit.Src = r
		r, err = s.Limit.Plan(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return r, nil
}

// Exec implements the stmt.Statement Exec interface.
func (s *UpdateStmt) Exec(ctx context.Context) (_ rset.Recordset, err error) {
	p, err := s.plan(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if err != nil {
		return nil, err
	}
	updatedRowKeys := make(map[string]bool)
	err = p.Do(ctx, func(id interface{}, in []interface{}) (more bool, err error) {
		// Update rows
		var rowKeys *plans.RowKeyList
		if in != nil && len(in) > 0 {
			t := in[len(in)-1]
			switch vt := t.(type) {
			case *plans.RowKeyList:
				rowKeys = vt
				in = in[:len(in)-1]
			}
		}
		if rowKeys == nil {
			// Nothing to update
			return true, nil
		}
		// Set EvalIdentFunc

		m := make(map[interface{}]interface{})
		m[expressions.ExprEvalIdentFunc] = func(name string) (interface{}, error) {
			return getIdentValue(name, p.GetFields(), in, field.DefaultFieldFlag)
		}
		// Update rows
		start := 0
		for _, entry := range rowKeys.Keys {
			tbl := entry.Tbl
			k := entry.Key
			_, ok := updatedRowKeys[k]
			if ok {
				// Each matching row is updated once, even if it matches the conditions multiple times.
				continue
			}
			// Update row
			id, uerr := util.DecodeHandleFromRowKey(k)
			if uerr != nil {
				return false, errors.Trace(uerr)
			}
			end := start + len(tbl.Cols())
			data := in[start:end]
			start = end
			tcols, uerr := getUpdateColumns(tbl, s.List, s.MultipleTable)
			if uerr != nil {
				return false, errors.Trace(uerr)
			}
			if len(tcols) == 0 {
				// Nothing to update for this table.
				continue
			}
			// Get data in the table
			uerr = updateRecord(ctx, id, data, tbl, tcols, s.List, nil, m)
			if uerr != nil {
				return false, errors.Trace(uerr)
			}
			updatedRowKeys[k] = true
		}
		return true, nil

	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return nil, nil
}

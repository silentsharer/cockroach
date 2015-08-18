// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"bytes"
	"fmt"
	"math"
	"sort"

	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/util/log"
)

// Select selects rows from a single table. Select is the workhorse of the SQL
// statements. In the slowest and most general case, select must perform full
// table scans across multiple tables and sort and join the resulting rows on
// arbitrary columns. Full table scans can be avoided when indexes can be used
// to satisfy the where-clause.
//
// Privileges: SELECT on table
//   Notes: postgres requires SELECT. Also requires UPDATE on "FOR UPDATE".
//          mysql requires SELECT.
func (p *planner) Select(n *parser.Select) (planNode, error) {
	s := &scanNode{txn: p.txn}
	if err := s.initFrom(p, n.From); err != nil {
		return nil, err
	}
	if err := s.initWhere(n.Where); err != nil {
		return nil, err
	}
	if err := s.initTargets(n.Exprs); err != nil {
		return nil, err
	}
	return p.selectIndex(s)
}

type subqueryVisitor struct {
	*planner
	err error
}

var _ parser.Visitor = &subqueryVisitor{}

func (v *subqueryVisitor) Visit(expr parser.Expr) parser.Expr {
	if v.err != nil {
		return expr
	}
	subquery, ok := expr.(*parser.Subquery)
	if !ok {
		return expr
	}
	var plan planNode
	if plan, v.err = v.makePlan(subquery.Select); v.err != nil {
		return expr
	}
	var rows parser.DTuple
	for plan.Next() {
		values := plan.Values()
		switch len(values) {
		case 1:
			// TODO(pmattis): This seems hokey, but if we don't do this then the
			// subquery expands to a tuple of tuples instead of a tuple of values and
			// an expression like "k IN (SELECT foo FROM bar)" will fail because
			// we're comparing a single value against a tuple. Perhaps comparison of
			// a single value against a tuple should succeed if the tuple is one
			// element in length.
			rows = append(rows, values[0])
		default:
			// The result from plan.Values() is only valid until the next call to
			// plan.Next(), so make a copy.
			valuesCopy := make(parser.DTuple, len(values))
			copy(valuesCopy, values)
			rows = append(rows, valuesCopy)
		}
	}
	v.err = plan.Err()
	return rows
}

func (p *planner) expandSubqueries(stmt parser.Statement) error {
	v := subqueryVisitor{planner: p}
	parser.WalkStmt(&v, stmt)
	return v.err
}

// selectIndex analyzes the scanNode to determine if there is an index
// available that can fulfill the query with a more restrictive scan.
//
// The current occurs in two passes. The first pass performs a limited form of
// value range propagation for the qvalues (i.e. the columns). The second pass
// takes the value range information and determines which indexes can fulfill
// the query (we currently only support covering indexes) and selects the
// "best" index from that set. The cost model based on keys per row, key size
// and number of index elements used.
func (p *planner) selectIndex(s *scanNode) (planNode, error) {
	if s.desc == nil || s.filter == nil {
		// No table or where-clause.
		return s, nil
	}

	// Analyze the filter expression to compute the qvalue range info.
	rangeInfo := make(qvalueRangeMap)
	rangeInfo.analyzeExpr(s.filter)

	candidates := make([]*indexInfo, 0, len(s.desc.Indexes)+1)

	if s.isSecondaryIndex {
		// An explicit secondary index was requested. Only add it to the candidate
		// indexes list.
		candidates = append(candidates, &indexInfo{
			desc:  s.desc,
			index: s.index,
		})
	} else {
		candidates = append(candidates, &indexInfo{
			desc:  s.desc,
			index: &s.desc.PrimaryIndex,
		})
		for i := range s.desc.Indexes {
			candidates = append(candidates, &indexInfo{
				desc:  s.desc,
				index: &s.desc.Indexes[i],
			})
		}
	}

	for _, c := range candidates {
		c.analyzeRanges(s, rangeInfo)
	}

	sort.Sort(indexInfoByCost(candidates))

	if log.V(2) {
		for i, c := range candidates {
			log.Infof("%d: selectIndex(%s): %v %s %s",
				i, c.index.Name, c.cost, c.makeStartKey(), c.makeEndKey())
		}
	}

	// After sorting, candidates[0] contains the best index. Copy its info into
	// the scanNode.
	s.index = candidates[0].index
	s.isSecondaryIndex = (s.index != &s.desc.PrimaryIndex)
	s.startKey = candidates[0].makeStartKey()
	s.endKey = candidates[0].makeEndKey()
	return s, nil
}

// qvalueInfo contains one end of a value range. Op is required to be either
// GE, GT, LE or LT.
type qvalueInfo struct {
	datum parser.Datum
	op    parser.ComparisonOp
}

func (q *qvalueInfo) assertStartOp() {
	switch q.op {
	case parser.GE, parser.GT:
	default:
		panic(fmt.Sprintf("unexpected start op: %s", q.op))
	}
}

func (q *qvalueInfo) assertEndOp() {
	switch q.op {
	case parser.LE, parser.LT:
	default:
		panic(fmt.Sprintf("unexpected end op: %s", q.op))
	}
}

func (q *qvalueInfo) compare(datum parser.Datum) int {
	// TODO(pmattis): We should really be comparing the datums directly, but this
	// is convenient for now.
	oldKey, err := encodeTableKey(nil, q.datum)
	if err != nil {
		panic(err)
	}
	newKey, err := encodeTableKey(nil, datum)
	if err != nil {
		panic(err)
	}
	return bytes.Compare(newKey, oldKey)
}

// intersect intersects a new value constraint with an existing constraint. The
// start parameter controls whether we intersect less than or greater than
// (start == true corresponds to greater than). Some examples:
//
//   q: >1  n: >2   out: >2
//   q: >1  n: >=1  out: >1
func (q *qvalueInfo) intersect(n qvalueInfo, start bool) {
	if start {
		n.assertStartOp()
	} else {
		n.assertEndOp()
	}

	if q.datum == nil {
		*q = n
		return
	}

	cmp := +1
	if !start {
		cmp = -1
	}

	switch q.compare(n.datum) {
	case cmp:
		*q = n
	case 0:
		if n.op == parser.GT || n.op == parser.LT {
			q.op = n.op
		}
	}
}

// union unions a new value constraint with an existing constraint. The start
// parameter controls whether we union less than or greater than (start == true
// corresponds to greater than). Perhaps best understood with some examples:
//
//  q: >1  n: >2   out: >1
//  q: >1  n: >=1  out: >=1
func (q *qvalueInfo) union(n qvalueInfo, start bool) {
	if start {
		n.assertStartOp()
	} else {
		n.assertEndOp()
	}

	if q.datum == nil {
		*q = n
		return
	}

	cmp := -1
	if !start {
		cmp = +1
	}

	switch q.compare(n.datum) {
	case cmp:
		*q = n
	case 0:
		if n.op == parser.GE || n.op == parser.LE {
			q.op = n.op
		}
	}
}

// qvalueRange represents the range of values a qvalue may have. start must be
// less than end. Note that whether the endpoints are inclusive or exclusive is
// determined by {start,end}.op.
type qvalueRange struct {
	start qvalueInfo
	end   qvalueInfo
}

type qvalueRangeMap map[ColumnID]*qvalueRange

// analyzeExpr walks over the specified expression and populates the range map
// with the value ranges for each qvalue.
func (m qvalueRangeMap) analyzeExpr(expr parser.Expr) {
	switch t := expr.(type) {
	case *parser.NotExpr:
		// TODO(pmattis): Similar to OR expressions, we can compute the value range
		// for the expression and then invert the results.

	case *parser.OrExpr:
		// Conjunctions are handled below (see *parser.AndExpr). Disjunctions are
		// handled by creating separate range maps for the left and right sides and
		// then combining them. For example:
		//
		//   "x < y OR x < z"   -> "x < MAX(y, z)"
		//   "x = y OR x = z"   -> "x >= y OR x <= z"
		//   "x < y OR x > z"   -> "t"
		left := make(qvalueRangeMap)
		left.analyzeExpr(t.Left)
		right := make(qvalueRangeMap)
		right.analyzeExpr(t.Right)

		for id, lRange := range left {
			rRange, ok := right[id]
			if !ok {
				continue
			}
			r := &qvalueRange{}
			if lRange.start.datum != nil && rRange.start.datum != nil {
				r.start = lRange.start
				r.start.union(rRange.start, true)
			}
			if lRange.end.datum != nil && rRange.end.datum != nil {
				r.end = lRange.end
				r.end.union(rRange.end, false)
			}
			if r.start.datum != nil || r.end.datum != nil {
				m[id] = r
			}
		}

	case *parser.AndExpr:
		m.analyzeExpr(t.Left)
		m.analyzeExpr(t.Right)

	case *parser.ParenExpr:
		m.analyzeExpr(t.Expr)

	case *parser.ComparisonExpr:
		m.analyzeComparisonExpr(t)

	case *parser.RangeCond:
		if t.Not {
			// "a NOT BETWEEN b AND c" -> "a < b OR a > c"
			m.analyzeExpr(&parser.OrExpr{
				Left: &parser.ComparisonExpr{
					Operator: parser.LT,
					Left:     t.Left,
					Right:    t.From,
				},
				Right: &parser.ComparisonExpr{
					Operator: parser.GT,
					Left:     t.Left,
					Right:    t.To,
				},
			})
		} else {
			// "a BETWEEN b AND c" -> "a >= b AND a <= c"
			m.analyzeExpr(&parser.AndExpr{
				Left: &parser.ComparisonExpr{
					Operator: parser.GE,
					Left:     t.Left,
					Right:    t.From,
				},
				Right: &parser.ComparisonExpr{
					Operator: parser.LE,
					Left:     t.Left,
					Right:    t.To,
				},
			})
		}
	}
}

// analyzeComparisonExpr analyzes the comparison expression, restricting the
// start and end info for any qvalues found within it.
func (m qvalueRangeMap) analyzeComparisonExpr(node *parser.ComparisonExpr) {
	op := node.Operator
	switch op {
	case parser.EQ, parser.LT, parser.LE, parser.GT, parser.GE:
		break

	default:
		// TODO(pmattis): For parser.LIKE we could extract the constant prefix and
		// treat as a range restriction.
		return
	}

	qval, ok := node.Left.(*qvalue)
	var constExpr parser.Expr
	if ok {
		if !isConst(node.Right) {
			// qvalue <op> non-constant.
			return
		}
		constExpr = node.Right
		// qval <op> constant.
	} else {
		qval, ok = node.Right.(*qvalue)
		if !ok {
			// non-qvalue <op> non-qvalue.
			return
		}
		if !isConst(node.Left) {
			// non-constant <op> qvalue.
			return
		}
		constExpr = node.Left
		// constant <op> qval: invert the operation.
		switch op {
		case parser.LT:
			op = parser.GT
		case parser.LE:
			op = parser.GE
		case parser.GT:
			op = parser.LT
		case parser.GE:
			op = parser.LE
		}
	}

	// Evaluate the constant expression.
	datum, err := parser.EvalExpr(constExpr)
	if err != nil {
		// TODO(pmattis): Should we pass the error up the stack?
		return
	}

	// Ensure the resulting datum matches the column type.
	if _, err := convertDatum(qval.col, datum); err != nil {
		// TODO(pmattis): Should we pass the error up the stack?
		return
	}

	if log.V(2) {
		log.Infof("analyzeComparisonExpr: %s %s %s",
			qval.col.Name, op, datum)
	}

	r := m.getRange(qval.col.ID)

	// Restrict the start element.
	switch startOp := op; startOp {
	case parser.EQ:
		startOp = parser.GE
		fallthrough
	case parser.GE, parser.GT:
		r.start.intersect(qvalueInfo{datum, startOp}, true)
	}

	// Restrict the end element.
	switch endOp := op; endOp {
	case parser.EQ:
		endOp = parser.LE
		fallthrough
	case parser.LE, parser.LT:
		r.end.intersect(qvalueInfo{datum, endOp}, false)
	}
}

func (m qvalueRangeMap) getRange(colID ColumnID) *qvalueRange {
	r := m[colID]
	if r == nil {
		r = &qvalueRange{}
		m[colID] = r
	}
	return r
}

type indexInfo struct {
	desc  *TableDescriptor
	index *IndexDescriptor
	start []qvalueInfo
	end   []qvalueInfo
	cost  float64
}

// analyzeRanges analyzes the scanNode and range map to determine the cost of
// using the index.
func (v *indexInfo) analyzeRanges(s *scanNode, m qvalueRangeMap) {
	if !v.isCoveringIndex(s.qvals) {
		// TODO(pmattis): Support non-coverying indexes.
		v.cost = math.MaxFloat64
		return
	}

	v.makeStartInfo(m)
	v.makeEndInfo(m)

	// The base cost is the number of keys per row.
	if v.index == &v.desc.PrimaryIndex {
		// The primary index contains 1 key per column plus the sentinel key per
		// row.
		v.cost = float64(1 + len(v.desc.Columns) - len(v.desc.PrimaryIndex.ColumnIDs))
	} else {
		v.cost = float64(len(v.index.ColumnIDs))
	}

	// Count the number of elements used to limit the start and end keys. We then
	// boost the cost by what fraction of the index keys are being used. The
	// higher the fraction, the lower the cost.
	if len(v.start) == 0 && len(v.end) == 0 {
		// The index isn't being restricted at all, bump the cost significantly to
		// make any index which does restrict the keys more desirable.
		v.cost *= 1000
	} else {
		v.cost *= float64(len(v.index.ColumnIDs)+len(v.index.ColumnIDs)) /
			float64(len(v.start)+len(v.end))
	}
}

func (v *indexInfo) makeStartInfo(m qvalueRangeMap) {
	v.start = make([]qvalueInfo, 0, len(v.index.ColumnIDs))
	for _, colID := range v.index.ColumnIDs {
		if r := m[colID]; r != nil {
			v.start = append(v.start, r.start)
			if r.start.op == parser.GE {
				continue
			}
		}
		break
	}
}

func (v *indexInfo) makeEndInfo(m qvalueRangeMap) {
	v.end = make([]qvalueInfo, 0, len(v.index.ColumnIDs))
	for _, colID := range v.index.ColumnIDs {
		if r := m[colID]; r != nil {
			v.end = append(v.end, r.end)
			if r.end.op == parser.LE {
				continue
			}
		}
		break
	}
}

func (v *indexInfo) makeStartKey() proto.Key {
	key := proto.Key(MakeIndexKeyPrefix(v.desc.ID, v.index.ID))
	for _, e := range v.start {
		if e.datum == nil {
			break
		}
		var err error
		key, err = encodeTableKey(key, e.datum)
		if err != nil {
			panic(err)
		}
		if e.op == parser.GT {
			// "qval > constant": we can't use any of the additional elements for
			// restricting the key further.
			key = key.Next()
			break
		}
	}
	return key
}

func (v *indexInfo) makeEndKey() proto.Key {
	key := proto.Key(MakeIndexKeyPrefix(v.desc.ID, v.index.ID))
	isLT := false
	for _, e := range v.end {
		if e.datum == nil {
			break
		}
		var err error
		key, err = encodeTableKey(key, e.datum)
		if err != nil {
			panic(err)
		}
		isLT = e.op == parser.LT
		if isLT {
			// "qval < constant": we can't use any of the additional elements for
			// restricting the key further.
			break
		}
	}
	if !isLT {
		// If the last element was not "qval < constant" then we want the
		// "prefix-end" for the end key.
		key = key.PrefixEnd()
	}
	return key
}

// coveringIndex returns true if all of the columns referenced by the target
// expressions and where clause are contained within the index. This allows a
// scan of only the index to be performed without requiring subsequent lookup
// of the full row.
func (v *indexInfo) isCoveringIndex(qvals qvalMap) bool {
	if v.index == &v.desc.PrimaryIndex {
		// The primary key index always covers all of the columns.
		return true
	}

	for colID := range qvals {
		if !v.index.containsColumnID(colID) &&
			(v.index.Unique || !v.desc.PrimaryIndex.containsColumnID(colID)) {
			return false
		}
	}
	return true
}

type isConstVisitor struct {
	isConst bool
}

var _ parser.Visitor = &isConstVisitor{}

func (v *isConstVisitor) Visit(expr parser.Expr) parser.Expr {
	if v.isConst {
		if _, ok := expr.(parser.DReference); ok {
			v.isConst = false
		}
	}
	return expr
}

// isConst returns true if the expression contains only constant values
// (i.e. it does not contain a DReference).
func isConst(expr parser.Expr) bool {
	v := isConstVisitor{isConst: true}
	expr = parser.WalkExpr(&v, expr)
	return v.isConst
}

type indexInfoByCost []*indexInfo

func (v indexInfoByCost) Len() int {
	return len(v)
}

func (v indexInfoByCost) Less(i, j int) bool {
	return v[i].cost < v[j].cost
}

func (v indexInfoByCost) Swap(i, j int) {
	v[i], v[j] = v[j], v[i]
}

/*
Copyright 2020 The Vitess Authors.

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

package planbuilder

import (
	"fmt"
	"io"
	"sort"

	"vitess.io/vitess/go/vt/vtgate/planbuilder/abstract"

	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vtgate/semantics"
	"vitess.io/vitess/go/vt/vtgate/vindexes"

	"vitess.io/vitess/go/vt/vterrors"

	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
)

var _ selectPlanner = gen4Planner

func gen4Planner(_ string) func(sqlparser.Statement, *sqlparser.ReservedVars, ContextVSchema) (engine.Primitive, error) {
	return func(stmt sqlparser.Statement, reservedVars *sqlparser.ReservedVars, vschema ContextVSchema) (engine.Primitive, error) {
		sel, ok := stmt.(*sqlparser.Select)
		if !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "%T not yet supported", stmt)
		}

		getPlan := func(sel *sqlparser.Select) (logicalPlan, error) {
			return newBuildSelectPlan(sel, reservedVars, vschema)
		}

		plan, err := getPlan(sel)
		if err != nil {
			return nil, err
		}

		if shouldRetryWithCNFRewriting(plan) {
			// by transforming the predicates to CNF, the planner will sometimes find better plans
			primitive := rewriteToCNFAndReplan(stmt, getPlan)
			if primitive != nil {
				return primitive, nil
			}
		}
		return plan.Primitive(), nil
	}
}

type postProcessor struct {
	inDerived bool
	semTable  *semantics.SemTable
	vschema   ContextVSchema
}

func (pp *postProcessor) planHorizon(plan logicalPlan, sel *sqlparser.Select) (logicalPlan, error) {
	hp := horizonPlanning{
		sel:       sel,
		plan:      plan,
		semTable:  pp.semTable,
		vschema:   pp.vschema,
		inDerived: pp.inDerived,
	}

	plan, err := hp.planHorizon()
	if err != nil {
		return nil, err
	}

	plan, err = planLimit(sel.Limit, plan)
	if err != nil {
		return nil, err
	}
	return plan, nil

}

func newBuildSelectPlan(sel *sqlparser.Select, reservedVars *sqlparser.ReservedVars, vschema ContextVSchema) (logicalPlan, error) {
	ksName := ""
	if ks, _ := vschema.DefaultKeyspace(); ks != nil {
		ksName = ks.Name
	}
	semTable, err := semantics.Analyze(sel, ksName, vschema)
	if err != nil {
		return nil, err
	}

	sel, err = rewrite(sel, semTable, reservedVars)
	if err != nil {
		return nil, err
	}

	opTree, err := abstract.CreateOperatorFromSelect(sel, semTable)
	if err != nil {
		return nil, err
	}

	ctx := optimizeContext{
		reservedVars: reservedVars,
		semTable:     semTable,
		vschema:      vschema,
	}
	tree, err := optimizeQuery(ctx, opTree)
	if err != nil {
		return nil, err
	}

	postProcessing := &postProcessor{
		semTable: semTable,
		vschema:  vschema,
	}
	plan, err := transformToLogicalPlan(tree, semTable, postProcessing)
	if err != nil {
		return nil, err
	}

	plan, err = postProcessing.planHorizon(plan, sel)
	if err != nil {
		return nil, err
	}

	if err := plan.WireupGen4(semTable); err != nil {
		return nil, err
	}

	directives := sqlparser.ExtractCommentDirectives(sel.Comments)
	if directives.IsSet(sqlparser.DirectiveScatterErrorsAsWarnings) {
		visit(plan, func(logicalPlan logicalPlan) (bool, logicalPlan, error) {
			switch plan := logicalPlan.(type) {
			case *route:
				plan.eroute.ScatterErrorsAsWarnings = true
			}
			return true, logicalPlan, nil
		})
	}

	return plan, nil
}

type optimizeContext struct {
	reservedVars *sqlparser.ReservedVars
	semTable     *semantics.SemTable
	vschema      ContextVSchema
}

func optimizeQuery(ctx optimizeContext, opTree abstract.Operator) (queryTree, error) {
	switch op := opTree.(type) {
	case *abstract.QueryGraph:
		switch {
		case ctx.vschema.Planner() == Gen4Left2Right:
			return leftToRightSolve(ctx, op)
		default:
			return greedySolve(ctx, op)
		}
	case *abstract.LeftJoin:
		treeInner, err := optimizeQuery(ctx, op.Left)
		if err != nil {
			return nil, err
		}
		treeOuter, err := optimizeQuery(ctx, op.Right)
		if err != nil {
			return nil, err
		}
		return mergeOrJoin(ctx, treeInner, treeOuter, []sqlparser.Expr{op.Predicate}, false)
	case *abstract.Join:
		treeInner, err := optimizeQuery(ctx, op.LHS)
		if err != nil {
			return nil, err
		}
		treeOuter, err := optimizeQuery(ctx, op.RHS)
		if err != nil {
			return nil, err
		}
		return mergeOrJoin(ctx, treeInner, treeOuter, []sqlparser.Expr{op.Exp}, true)
	case *abstract.Derived:
		treeInner, err := optimizeQuery(ctx, op.Inner)
		if err != nil {
			return nil, err
		}
		return &derivedTree{
			query: op.Sel,
			inner: treeInner,
			alias: op.Alias,
		}, nil
	case *abstract.SubQuery:
		outerTree, err := optimizeQuery(ctx, op.Outer)
		if err != nil {
			return nil, err
		}
		var unmerged []*subqueryTree

		// first loop over the subqueries and try to merge them into the outer plan
		for _, inner := range op.Inner {
			treeInner, err := optimizeQuery(ctx, inner.Inner)
			if err != nil {
				return nil, err
			}
			canMerge, err := canMergeSubQuery(outerTree, treeInner, inner.Inner)
			if err != nil {
				return nil, err
			}
			if canMerge {
				outerTree = mergeSubQuery(outerTree, inner)
			} else {
				unmerged = append(unmerged, &subqueryTree{
					subquery: inner.SelectStatement,
					inner:    treeInner,
					opcode:   inner.Type,
					argName:  inner.ArgName,
				})
			}
		}

		/*
			build a tree of the unmerged subqueries
			rt: route, sqt: subqueryTree


			            sqt
			         sqt   rt
			        rt rt
		*/

		for _, tree := range unmerged {
			tree.outer = outerTree
			outerTree = tree
		}

		return outerTree, nil
	default:
		return nil, semantics.Gen4NotSupportedF("optimizeQuery")
	}
}

// func tryMergeSubQuery(outer, subq queryTree, subqOp abstract.Operator) (queryTree, error) {
// 	outerRoute, subQRoute := joinTreesToRoutes(outer, subq)
// 	if outerRoute == nil || subQRoute == nil {
// 		return nil, nil
// 	}
// 	if outerRoute.keyspace != subQRoute.keyspace {
// 		if solves, _ := subqOp.Solves(outer.tableID()); solves {
// 			// throwing below error for compatibility
// 			return nil, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard correlated subquery")
// 		}
// 		return nil, nil
// 	}
// 	outerOpCode := outerRoute.routeOpCode
// 	subRouteOpCode := subQRoute.routeOpCode
// 	switch outerOpCode {
// 	case engine.SelectUnsharded, engine.SelectDBA:
// 		if subRouteOpCode != outerOpCode && subRouteOpCode != engine.SelectReference {
// 			return nil, nil
// 		}
// 	case engine.SelectEqualUnique:
//
// 		matchVdxName, err := isMatchingVindexName(outer, subq)
// 		if err != nil {
// 			return nil, nil
// 		}
// 		matchVdxValue, err := isMatchingVindexValue(outer, subq)
// 		if err != nil {
// 			return nil, nil
// 		}
// 		if subRouteOpCode == outerOpCode && matchVdxName && matchVdxValue {
//
// 		} else {
// 			return nil, nil
// 		}
// 	case engine.SelectNext:
// 		return nil, nil
// 	case engine.SelectReference:
//
// 	}
//
// 	solves, exprs := subqOp.Solves(outer.tableID())
// 	if !solves {
// 		return nil, nil
// 	}
//
// 	outerVindexPreds := outerRoute.vindexPreds
// 	innerVindexPreds := subQRoute.vindexPreds
//
// }

func canMergeSubQuery(outer, subq queryTree, subqOp abstract.Operator) (bool, error) {
	ksMatch, err := isQueryTreeKeyspaceMatching(subq, outer)
	if err != nil {
		return false, nil
	}
	if !ksMatch {
		if solves, _ := subqOp.Solves(outer.tableID()); solves {
			// throwing below error for compatibility
			return false, vterrors.Errorf(vtrpcpb.Code_UNIMPLEMENTED, "unsupported: cross-shard correlated subquery")
		}
		return false, nil
	}
	subqOpCode, err := subq.getOpCode()
	if err != nil {
		return false, nil
	}
	outerOpCode, err := outer.getOpCode()
	if err != nil {
		return false, nil
	}

	switch outerOpCode {
	case engine.SelectUnsharded, engine.SelectDBA, engine.SelectReference:
		return subqOpCode == outerOpCode || subqOpCode == engine.SelectReference, nil
	case engine.SelectEqualUnique:
		matchVdxName, err := isMatchingVindexName(outer, subq)
		if err != nil {
			return false, nil
		}
		matchVdxValue, err := isMatchingVindexValue(outer, subq)
		if err != nil {
			return false, nil
		}
		return subqOpCode == outerOpCode && matchVdxName && matchVdxValue, nil
	case engine.SelectNext:
		return false, nil
	}

	// correlated
	if solves, exprs := subqOp.Solves(outer.tableID()); solves {
		outerVindexPreds, err := outer.getVindexPredicates()
		if err != nil {
			return false, nil
		}
		innerVindexPreds, err := subq.getVindexPredicates()
		if err != nil {
			return false, nil
		}

		for _, expr := range exprs {
			cmp, isCmp := expr.(*sqlparser.ComparisonExpr)
			if !isCmp {
				continue
			}

			for _, pred := range outerVindexPreds {
				for _, innerPred := range innerVindexPreds {
					if pred.colVindex.Name != innerPred.colVindex.Name {
						continue
					}
					if !cmp.ColNameMatch(pred.colVindex.Columns[0], innerPred.colVindex.Columns[0]) {
						return false, nil
					}
				}
			}
		}
		return true, nil
	}
	return false, nil
}

func mergeSubQuery(outer queryTree, subq *abstract.SubQueryInner) queryTree {
	switch outer := outer.(type) {
	case *routeTree:
		if outer.sqToReplace == nil {
			outer.sqToReplace = map[string]*sqlparser.Select{}
		}
		outer.sqToReplace[subq.ArgName] = subq.SelectStatement
		return outer
	case *subqueryTree:
		/*
			 	subq will only be able to merge with the outer.outer
				for that reason, we create a new queryTree that merges our outer.outer with our subq

							 outer
					      /			\
					     /			 \
					outer.outer	 outer.inner

					outer.outer = outer.outer + subq
		*/
		outer.outer = mergeSubQuery(outer.outer, subq)
		return outer
	}
	return nil
}

func planLimit(limit *sqlparser.Limit, plan logicalPlan) (logicalPlan, error) {
	if limit == nil {
		return plan, nil
	}
	rb, ok := plan.(*route)
	if ok && rb.isSingleShard() {
		rb.SetLimit(limit)
		return plan, nil
	}

	lPlan, err := createLimit(plan, limit)
	if err != nil {
		return nil, err
	}

	// visit does not modify the plan.
	_, err = visit(lPlan, setUpperLimit)
	if err != nil {
		return nil, err
	}
	return lPlan, nil
}

type horizonPlanning struct {
	sel             *sqlparser.Select
	plan            logicalPlan
	semTable        *semantics.SemTable
	vschema         ContextVSchema
	qp              *abstract.QueryProjection
	inDerived       bool
	needsTruncation bool
	vtgateGrouping  bool
}

func (hp *horizonPlanning) planHorizon() (logicalPlan, error) {
	rb, ok := hp.plan.(*route)
	if !ok && hp.semTable.ProjectionErr != nil {
		return nil, hp.semTable.ProjectionErr
	}

	if hp.inDerived {
		for _, expr := range hp.sel.SelectExprs {
			if sqlparser.ContainsAggregation(expr) {
				return nil, semantics.Gen4NotSupportedF("aggregation inside of derived table")
			}
		}
	}

	if ok && rb.isSingleShard() {
		createSingleShardRoutePlan(hp.sel, rb)
		return hp.plan, nil
	}

	qp2, err := abstract.CreateQPFromSelect(hp.sel)
	if err != nil {
		return nil, err
	}

	hp.qp = qp2

	if err := checkUnsupportedConstructs(hp.sel); err != nil {
		return nil, err
	}

	if hp.qp.NeedsAggregation() {
		err = hp.planAggregations()
		if err != nil {
			return nil, err
		}
	} else {
		for _, e := range hp.qp.SelectExprs {
			if _, _, err := pushProjection(e.Col, hp.plan, hp.semTable, true, false); err != nil {
				return nil, err
			}
		}
	}

	if len(hp.qp.OrderExprs) > 0 {
		hp.plan, err = hp.planOrderBy(hp.qp.OrderExprs, hp.plan)
		if err != nil {
			return nil, err
		}
	}

	if hp.qp.CanPushDownSorting && hp.vtgateGrouping {
		hp.plan, err = hp.planOrderByUsingGroupBy()
		if err != nil {
			return nil, err
		}
	}

	err = hp.planDistinct()
	if err != nil {
		return nil, err
	}

	err = hp.truncateColumnsIfNeeded(hp.plan)
	if err != nil {
		return nil, err
	}

	return hp.plan, nil
}

func (hp *horizonPlanning) truncateColumnsIfNeeded(plan logicalPlan) error {
	if !hp.needsTruncation {
		return nil
	}

	switch p := plan.(type) {
	case *route:
		p.eroute.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *joinGen4:
		// since this is a join, we can safely add extra columns and not need to truncate them
	case *orderedAggregate:
		p.eaggr.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *memorySort:
		p.truncater.SetTruncateColumnCount(hp.sel.GetColumnCount())
	case *pulloutSubquery:
		return hp.truncateColumnsIfNeeded(p.underlying)
	default:
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "plan type not known for column truncation: %T", hp.plan)
	}

	return nil
}

func exprHasUniqueVindex(vschema ContextVSchema, semTable *semantics.SemTable, expr sqlparser.Expr) bool {
	col, isCol := expr.(*sqlparser.ColName)
	if !isCol {
		return false
	}
	ts := semTable.BaseTableDependencies(expr)
	tableInfo, err := semTable.TableInfoFor(ts)
	if err != nil {
		return false
	}
	tableName, err := tableInfo.Name()
	if err != nil {
		return false
	}
	vschemaTable, _, _, _, _, err := vschema.FindTableOrVindex(tableName)
	if err != nil {
		return false
	}
	for _, vindex := range vschemaTable.ColumnVindexes {
		if len(vindex.Columns) > 1 || !vindex.Vindex.IsUnique() {
			return false
		}
		if col.Name.Equal(vindex.Columns[0]) {
			return true
		}
	}
	return false
}

func createSingleShardRoutePlan(sel *sqlparser.Select, rb *route) {
	ast := rb.Select.(*sqlparser.Select)
	ast.Distinct = sel.Distinct
	ast.GroupBy = sel.GroupBy
	ast.OrderBy = sel.OrderBy
	ast.Comments = sel.Comments
	ast.SelectExprs = sel.SelectExprs
	for i, expr := range ast.SelectExprs {
		if aliasedExpr, ok := expr.(*sqlparser.AliasedExpr); ok {
			ast.SelectExprs[i] = removeQualifierFromColName(aliasedExpr)
		}
	}
}

func checkUnsupportedConstructs(sel *sqlparser.Select) error {
	if sel.Having != nil {
		return semantics.Gen4NotSupportedF("HAVING")
	}
	return nil
}

func pushJoinPredicate(ctx optimizeContext, exprs []sqlparser.Expr, tree queryTree) (queryTree, error) {
	switch node := tree.(type) {
	case *routeTree:
		plan := node.clone().(*routeTree)
		err := plan.addPredicate(exprs...)
		if err != nil {
			return nil, err
		}
		return plan, nil

	case *joinTree:
		node = node.clone().(*joinTree)

		// we break up the predicates so that colnames from the LHS are replaced by arguments
		var rhsPreds []sqlparser.Expr
		var lhsColumns []*sqlparser.ColName
		var lhsVarsName []string
		lhsSolves := node.lhs.tableID()
		for _, expr := range exprs {
			bvName, cols, predicate, err := breakPredicateInLHSandRHS(expr, ctx.semTable, lhsSolves)
			if err != nil {
				return nil, err
			}
			lhsColumns = append(lhsColumns, cols...)
			lhsVarsName = append(lhsVarsName, bvName...)
			rhsPreds = append(rhsPreds, predicate)
		}
		if lhsColumns != nil && lhsVarsName != nil {
			idxs, err := node.pushOutputColumns(lhsColumns, ctx.semTable)
			if err != nil {
				return nil, err
			}
			for i, idx := range idxs {
				node.vars[lhsVarsName[i]] = idx
			}
		}

		rhsPlan, err := pushJoinPredicate(ctx, rhsPreds, node.rhs)
		if err != nil {
			return nil, err
		}

		return &joinTree{
			lhs:   node.lhs,
			rhs:   rhsPlan,
			outer: node.outer,
			vars:  node.vars,
		}, nil
	case *derivedTree:
		plan := node.clone().(*derivedTree)

		newExpressions := make([]sqlparser.Expr, 0, len(exprs))
		for _, expr := range exprs {
			tblInfo, err := ctx.semTable.TableInfoForExpr(expr)
			if err != nil {
				return nil, err
			}
			rewritten, err := semantics.RewriteDerivedExpression(expr, tblInfo)
			if err != nil {
				return nil, err
			}
			newExpressions = append(newExpressions, rewritten)
		}

		newInner, err := pushJoinPredicate(ctx, newExpressions, plan.inner)
		if err != nil {
			return nil, err
		}

		plan.inner = newInner
		return plan, nil
	default:
		panic(fmt.Sprintf("BUG: unknown type %T", node))
	}
}

func breakPredicateInLHSandRHS(expr sqlparser.Expr, semTable *semantics.SemTable, lhs semantics.TableSet) (bvNames []string, columns []*sqlparser.ColName, predicate sqlparser.Expr, err error) {
	predicate = sqlparser.CloneExpr(expr)
	_ = sqlparser.Rewrite(predicate, nil, func(cursor *sqlparser.Cursor) bool {
		switch node := cursor.Node().(type) {
		case *sqlparser.ColName:
			deps := semTable.BaseTableDependencies(node)
			if deps == 0 {
				err = vterrors.Errorf(vtrpcpb.Code_INTERNAL, "unknown column. has the AST been copied?")
				return false
			}
			if deps.IsSolvedBy(lhs) {
				node.Qualifier.Qualifier = sqlparser.NewTableIdent("")
				columns = append(columns, node)
				bvName := node.CompliantName()
				bvNames = append(bvNames, bvName)
				arg := sqlparser.NewArgument(bvName)
				cursor.Replace(arg)
			}
		}
		return true
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return
}

func mergeOrJoinInner(ctx optimizeContext, lhs, rhs queryTree, joinPredicates []sqlparser.Expr) (queryTree, error) {
	return mergeOrJoin(ctx, lhs, rhs, joinPredicates, true)
}

func mergeOrJoin(ctx optimizeContext, lhs, rhs queryTree, joinPredicates []sqlparser.Expr, inner bool) (queryTree, error) {
	newTabletSet := lhs.tableID() | rhs.tableID()

	merger := func(a, b *routeTree) *routeTree {
		if inner {
			return createRoutePlanForInner(a, b, newTabletSet, joinPredicates)
		}
		return createRoutePlanForOuter(ctx, a, b, newTabletSet, joinPredicates)
	}

	newPlan := tryMerge(ctx, lhs, rhs, joinPredicates, merger)
	if newPlan != nil {
		return newPlan, nil
	}

	tree := &joinTree{lhs: lhs.clone(), rhs: rhs.clone(), outer: !inner, vars: map[string]int{}}
	return pushJoinPredicate(ctx, joinPredicates, tree)
}

type (
	tableSetPair struct {
		left, right semantics.TableSet
	}
	cacheMap map[tableSetPair]queryTree
)

/*
	The greedy planner will plan a query by finding first finding the best route plan for every table.
    Then, iteratively, it finds the cheapest join that can be produced between the remaining plans,
	and removes the two inputs to this cheapest plan and instead adds the join.
	As an optimization, it first only considers joining tables that have predicates defined between them
*/
func greedySolve(ctx optimizeContext, qg *abstract.QueryGraph) (queryTree, error) {
	joinTrees, err := seedPlanList(ctx, qg)
	planCache := cacheMap{}
	if err != nil {
		return nil, err
	}

	tree, err := mergeJoinTrees(ctx, qg, joinTrees, planCache, false)
	if err != nil {
		return nil, err
	}
	return tree, nil
}

func mergeJoinTrees(ctx optimizeContext, qg *abstract.QueryGraph, joinTrees []queryTree, planCache cacheMap, crossJoinsOK bool) (queryTree, error) {
	if len(joinTrees) == 0 {
		return nil, nil
	}
	for len(joinTrees) > 1 {
		bestTree, lIdx, rIdx, err := findBestJoinTree(ctx, qg, joinTrees, planCache, crossJoinsOK)
		if err != nil {
			return nil, err
		}
		// if we found a best plan, we'll replace the two plans that were joined with the join plan created
		if bestTree != nil {
			// we need to remove the larger of the two plans first
			if rIdx > lIdx {
				joinTrees = removeAt(joinTrees, rIdx)
				joinTrees = removeAt(joinTrees, lIdx)
			} else {
				joinTrees = removeAt(joinTrees, lIdx)
				joinTrees = removeAt(joinTrees, rIdx)
			}
			joinTrees = append(joinTrees, bestTree)
		} else {
			// we will only fail to find a join plan when there are only cross joins left
			// when that happens, we switch over to allow cross joins as well.
			// this way we prioritize joining joinTrees with predicates first
			crossJoinsOK = true
		}
	}
	return joinTrees[0], nil
}

func (cm cacheMap) getJoinTreeFor(ctx optimizeContext, lhs, rhs queryTree, joinPredicates []sqlparser.Expr) (queryTree, error) {
	solves := tableSetPair{left: lhs.tableID(), right: rhs.tableID()}
	cachedPlan := cm[solves]
	if cachedPlan != nil {
		return cachedPlan, nil
	}

	join, err := mergeOrJoinInner(ctx, lhs, rhs, joinPredicates)
	if err != nil {
		return nil, err
	}
	cm[solves] = join
	return join, nil
}

func findBestJoinTree(
	ctx optimizeContext,
	qg *abstract.QueryGraph,
	plans []queryTree,
	planCache cacheMap,
	crossJoinsOK bool,
) (bestPlan queryTree, lIdx int, rIdx int, err error) {
	for i, lhs := range plans {
		for j, rhs := range plans {
			if i == j {
				continue
			}
			joinPredicates := qg.GetPredicates(lhs.tableID(), rhs.tableID())
			if len(joinPredicates) == 0 && !crossJoinsOK {
				// if there are no predicates joining the two tables,
				// creating a join between them would produce a
				// cartesian product, which is almost always a bad idea
				continue
			}
			plan, err := planCache.getJoinTreeFor(ctx, lhs, rhs, joinPredicates)
			if err != nil {
				return nil, 0, 0, err
			}
			if bestPlan == nil || plan.cost() < bestPlan.cost() {
				bestPlan = plan
				// remember which plans we based on, so we can remove them later
				lIdx = i
				rIdx = j
			}
		}
	}
	return bestPlan, lIdx, rIdx, nil
}

func leftToRightSolve(ctx optimizeContext, qg *abstract.QueryGraph) (queryTree, error) {
	plans, err := seedPlanList(ctx, qg)
	if err != nil {
		return nil, err
	}

	var acc queryTree
	for _, plan := range plans {
		if acc == nil {
			acc = plan
			continue
		}
		joinPredicates := qg.GetPredicates(acc.tableID(), plan.tableID())
		acc, err = mergeOrJoinInner(ctx, acc, plan, joinPredicates)
		if err != nil {
			return nil, err
		}
	}

	return acc, nil
}

// seedPlanList returns a routeTree for each table in the qg
func seedPlanList(ctx optimizeContext, qg *abstract.QueryGraph) ([]queryTree, error) {
	plans := make([]queryTree, len(qg.Tables))

	// we start by seeding the table with the single routes
	for i, table := range qg.Tables {
		solves := ctx.semTable.TableSetFor(table.Alias)
		plan, err := createRoutePlan(table, solves, ctx.reservedVars, ctx.vschema)
		if err != nil {
			return nil, err
		}
		if qg.NoDeps != nil {
			plan.predicates = append(plan.predicates, sqlparser.SplitAndExpression(nil, qg.NoDeps)...)
		}
		plans[i] = plan
	}
	return plans, nil
}

func removeAt(plans []queryTree, idx int) []queryTree {
	return append(plans[:idx], plans[idx+1:]...)
}

func createRoutePlan(table *abstract.QueryTable, solves semantics.TableSet, reservedVars *sqlparser.ReservedVars, vschema ContextVSchema) (*routeTree, error) {
	if table.IsInfSchema {
		ks, err := vschema.AnyKeyspace()
		if err != nil {
			return nil, err
		}
		rp := &routeTree{
			routeOpCode: engine.SelectDBA,
			solved:      solves,
			keyspace:    ks,
			tables: []relation{&routeTable{
				qtable: table,
				vtable: &vindexes.Table{
					Name:     table.Table.Name,
					Keyspace: ks,
				},
			}},
			predicates: table.Predicates,
		}
		err = rp.findSysInfoRoutingPredicatesGen4(reservedVars)
		if err != nil {
			return nil, err
		}

		return rp, nil
	}
	vschemaTable, _, _, _, _, err := vschema.FindTableOrVindex(table.Table)
	if err != nil {
		return nil, err
	}
	if vschemaTable.Name.String() != table.Table.Name.String() {
		// we are dealing with a routed table
		name := table.Table.Name
		table.Table.Name = vschemaTable.Name
		astTable, ok := table.Alias.Expr.(sqlparser.TableName)
		if !ok {
			return nil, vterrors.Errorf(vtrpcpb.Code_INTERNAL, "[BUG] a derived table should never be a routed table")
		}
		realTableName := sqlparser.NewTableIdent(vschemaTable.Name.String())
		astTable.Name = realTableName
		if table.Alias.As.IsEmpty() {
			// if the user hasn't specified an alias, we'll insert one here so the old table name still works
			table.Alias.As = sqlparser.NewTableIdent(name.String())
		}
	}
	plan := &routeTree{
		solved: solves,
		tables: []relation{&routeTable{
			qtable: table,
			vtable: vschemaTable,
		}},
		keyspace: vschemaTable.Keyspace,
	}

	for _, columnVindex := range vschemaTable.ColumnVindexes {
		plan.vindexPreds = append(plan.vindexPreds, &vindexPlusPredicates{colVindex: columnVindex})
	}

	switch {
	case vschemaTable.Type == vindexes.TypeSequence:
		plan.routeOpCode = engine.SelectNext
	case vschemaTable.Type == vindexes.TypeReference:
		plan.routeOpCode = engine.SelectReference
	case !vschemaTable.Keyspace.Sharded:
		plan.routeOpCode = engine.SelectUnsharded
	case vschemaTable.Pinned != nil:

		// Pinned tables have their keyspace ids already assigned.
		// Use the Binary vindex, which is the identity function
		// for keyspace id.
		plan.routeOpCode = engine.SelectEqualUnique
	default:
		plan.routeOpCode = engine.SelectScatter
	}
	err = plan.addPredicate(table.Predicates...)
	if err != nil {
		return nil, err
	}

	return plan, nil
}

func findColumnVindex(ctx optimizeContext, a *routeTree, exp sqlparser.Expr) vindexes.SingleColumn {
	left, isCol := exp.(*sqlparser.ColName)
	if !isCol {
		return nil
	}
	leftDep := ctx.semTable.BaseTableDependencies(left)

	var singCol vindexes.SingleColumn

	_ = visitRelations(a.tables, func(rel relation) (bool, error) {
		rb, isRoute := rel.(*routeTable)
		if !isRoute {
			return true, nil
		}
		if leftDep.IsSolvedBy(rb.qtable.TableID) {
			for _, vindex := range rb.vtable.ColumnVindexes {
				sC, isSingle := vindex.Vindex.(vindexes.SingleColumn)
				if isSingle && vindex.Columns[0].Equal(left.Name) {
					singCol = sC
					return false, io.EOF
				}
			}
		}
		return false, nil
	})

	return singCol
}

func canMergeOnFilter(ctx optimizeContext, a, b *routeTree, predicate sqlparser.Expr) bool {
	comparison, ok := predicate.(*sqlparser.ComparisonExpr)
	if !ok {
		return false
	}
	if comparison.Operator != sqlparser.EqualOp {
		return false
	}
	left := comparison.Left
	right := comparison.Right

	lVindex := findColumnVindex(ctx, a, left)
	if lVindex == nil {
		left, right = right, left
		lVindex = findColumnVindex(ctx, a, left)
	}
	if lVindex == nil || !lVindex.IsUnique() {
		return false
	}
	rVindex := findColumnVindex(ctx, b, right)
	if rVindex == nil {
		return false
	}
	return rVindex == lVindex
}

func canMergeOnFilters(ctx optimizeContext, a, b *routeTree, joinPredicates []sqlparser.Expr) bool {
	for _, predicate := range joinPredicates {
		for _, expr := range sqlparser.SplitAndExpression(nil, predicate) {
			if canMergeOnFilter(ctx, a, b, expr) {
				return true
			}
		}
	}
	return false
}

type mergeFunc func(a, b *routeTree) *routeTree

func tryMerge(ctx optimizeContext, a, b queryTree, joinPredicates []sqlparser.Expr, merger mergeFunc) queryTree {
	aRoute, bRoute := joinTreesToRoutes(a, b)
	if aRoute == nil || bRoute == nil {
		return nil
	}
	// If both the routes are system schema queries then we do not validate the keyspaces at plan time. As they are not always the ones where the query will be sent to.
	if (aRoute.routeOpCode != engine.SelectDBA ||
		bRoute.routeOpCode != engine.SelectDBA) && aRoute.keyspace != bRoute.keyspace {
		return nil
	}

	r := merger(aRoute, bRoute)

	switch aRoute.routeOpCode {
	case engine.SelectUnsharded, engine.SelectDBA:
		if aRoute.routeOpCode != bRoute.routeOpCode {
			return nil
		}
	case engine.SelectScatter, engine.SelectEqualUnique:
		if bRoute.routeOpCode == engine.SelectReference {
			return r
		}
		if len(joinPredicates) == 0 {
			// If we are doing two Scatters, we have to make sure that the
			// joins are on the correct vindex to allow them to be merged
			// no join predicates - no vindex
			return nil
		}

		canMerge := canMergeOnFilters(ctx, aRoute, bRoute, joinPredicates)
		if !canMerge {
			return nil
		}
		r.pickBestAvailableVindex()
	case engine.SelectReference:
		if bRoute.routeOpCode != engine.SelectReference {
			return nil
		}
	}
	return r
}

func makeRoute(j queryTree) *routeTree {
	rb, ok := j.(*routeTree)
	if ok {
		return rb
	}

	x, ok := j.(*derivedTree)
	if !ok {
		return nil
	}
	dp := x.clone().(*derivedTree)

	inner := makeRoute(dp.inner)
	if inner == nil {
		return nil
	}

	dt := &derivedTable{
		tables:     inner.tables,
		query:      dp.query,
		predicates: inner.predicates,
		leftJoins:  inner.leftJoins,
		alias:      dp.alias,
	}

	inner.tables = parenTables{dt}
	inner.predicates = nil
	inner.leftJoins = nil
	return inner
}

func joinTreesToRoutes(a, b queryTree) (*routeTree, *routeTree) {
	aRoute := makeRoute(a)
	if aRoute == nil {
		return nil, nil
	}
	bRoute := makeRoute(b)
	if bRoute == nil {
		return nil, nil
	}
	return aRoute, bRoute
}

func createRoutePlanForInner(aRoute *routeTree, bRoute *routeTree, newTabletSet semantics.TableSet, joinPredicates []sqlparser.Expr) *routeTree {
	var tables parenTables
	if !aRoute.hasOuterjoins() {
		tables = append(aRoute.tables, bRoute.tables...)
	} else {
		tables = append(parenTables{aRoute.tables}, bRoute.tables...)
	}

	// append system table names from both the routes.
	sysTableName := aRoute.SysTableTableName
	if sysTableName == nil {
		sysTableName = bRoute.SysTableTableName
	} else {
		for k, v := range bRoute.SysTableTableName {
			sysTableName[k] = v
		}
	}

	return &routeTree{
		routeOpCode: aRoute.routeOpCode,
		solved:      newTabletSet,
		tables:      tables,
		predicates: append(
			append(aRoute.predicates, bRoute.predicates...),
			joinPredicates...),
		keyspace:            aRoute.keyspace,
		vindexPreds:         append(aRoute.vindexPreds, bRoute.vindexPreds...),
		leftJoins:           append(aRoute.leftJoins, bRoute.leftJoins...),
		SysTableTableSchema: append(aRoute.SysTableTableSchema, bRoute.SysTableTableSchema...),
		SysTableTableName:   sysTableName,
	}
}

func findTables(deps semantics.TableSet, tables parenTables) (relation, relation, parenTables) {
	foundTables := parenTables{}
	newTables := parenTables{}

	for i, t := range tables {
		if t.tableID().IsSolvedBy(deps) {
			foundTables = append(foundTables, t)
			if len(foundTables) == 2 {
				return foundTables[0], foundTables[1], append(newTables, tables[i:])
			}
		} else {
			newTables = append(newTables, t)
		}
	}
	return nil, nil, tables
}

func createRoutePlanForOuter(ctx optimizeContext, aRoute, bRoute *routeTree, newTabletSet semantics.TableSet, joinPredicates []sqlparser.Expr) *routeTree {
	// create relation slice with all tables
	tables := bRoute.tables
	// we are doing an outer join where the outer part contains multiple tables - we have to turn the outer part into a join or two
	for _, predicate := range bRoute.predicates {
		deps := ctx.semTable.BaseTableDependencies(predicate)
		aTbl, bTbl, newTables := findTables(deps, tables)
		tables = newTables
		if aTbl != nil && bTbl != nil {
			tables = append(tables, &joinTables{
				lhs:  aTbl,
				rhs:  bTbl,
				pred: predicate,
			})
		}
	}

	var outer relation
	if len(tables) == 1 {
		// if we have a single relation, no need to put it inside parens
		outer = tables[0]
	} else {
		outer = tables
	}

	return &routeTree{
		routeOpCode: aRoute.routeOpCode,
		solved:      newTabletSet,
		tables:      aRoute.tables,
		leftJoins: append(aRoute.leftJoins, &outerTable{
			right: outer,
			pred:  sqlparser.AndExpressions(joinPredicates...),
		}),
		keyspace:    aRoute.keyspace,
		vindexPreds: append(aRoute.vindexPreds, bRoute.vindexPreds...),
	}
}

var _ sort.Interface = (parenTables)(nil)

func (p parenTables) Len() int {
	return len(p)
}

func (p parenTables) Less(i, j int) bool {
	return p[i].tableID() < p[j].tableID()
}

func (p parenTables) Swap(i, j int) {
	p[i], p[j] = p[j], p[i]
}

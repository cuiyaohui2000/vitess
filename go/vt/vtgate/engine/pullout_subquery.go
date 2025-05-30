/*
Copyright 2019 The Vitess Authors.

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

package engine

import (
	"fmt"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/vterrors"

	querypb "vitess.io/vitess/go/vt/proto/query"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

var _ Primitive = (*PulloutSubquery)(nil)

// PulloutSubquery executes a "pulled out" subquery and stores
// the results in a bind variable.
type PulloutSubquery struct {
	Opcode PulloutOpcode

	// SubqueryResult and HasValues are used to send in the bindvar used in the query to the underlying primitive
	SubqueryResult string
	HasValues      string

	Subquery   Primitive
	Underlying Primitive
}

// Inputs returns the input primitives for this join
func (ps *PulloutSubquery) Inputs() []Primitive {
	return []Primitive{ps.Subquery, ps.Underlying}
}

// RouteType returns a description of the query routing type used by the primitive
func (ps *PulloutSubquery) RouteType() string {
	return ps.Opcode.String()
}

// GetKeyspaceName specifies the Keyspace that this primitive routes to.
func (ps *PulloutSubquery) GetKeyspaceName() string {
	return ps.Underlying.GetKeyspaceName()
}

// GetTableName specifies the table that this primitive routes to.
func (ps *PulloutSubquery) GetTableName() string {
	return ps.Underlying.GetTableName()
}

// TryExecute satisfies the Primitive interface.
func (ps *PulloutSubquery) TryExecute(vcursor VCursor, bindVars map[string]*querypb.BindVariable, wantfields bool) (*sqltypes.Result, error) {
	combinedVars, err := ps.execSubquery(vcursor, bindVars)
	if err != nil {
		return nil, err
	}
	return vcursor.ExecutePrimitive(ps.Underlying, combinedVars, wantfields)
}

// TryStreamExecute performs a streaming exec.
func (ps *PulloutSubquery) TryStreamExecute(vcursor VCursor, bindVars map[string]*querypb.BindVariable, wantfields bool, callback func(*sqltypes.Result) error) error {
	combinedVars, err := ps.execSubquery(vcursor, bindVars)
	if err != nil {
		return err
	}
	return vcursor.StreamExecutePrimitive(ps.Underlying, combinedVars, wantfields, callback)
}

// GetFields fetches the field info.
func (ps *PulloutSubquery) GetFields(vcursor VCursor, bindVars map[string]*querypb.BindVariable) (*sqltypes.Result, error) {
	combinedVars := make(map[string]*querypb.BindVariable, len(bindVars)+1)
	for k, v := range bindVars {
		combinedVars[k] = v
	}
	switch ps.Opcode {
	case PulloutValue:
		combinedVars[ps.SubqueryResult] = sqltypes.NullBindVariable
	case PulloutIn, PulloutNotIn:
		combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(0)
		combinedVars[ps.SubqueryResult] = &querypb.BindVariable{
			Type:   querypb.Type_TUPLE,
			Values: []*querypb.Value{sqltypes.ValueToProto(sqltypes.NewInt64(0))},
		}
	case PulloutExists:
		combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(0)
	}
	return ps.Underlying.GetFields(vcursor, combinedVars)
}

// NeedsTransaction implements the Primitive interface
func (ps *PulloutSubquery) NeedsTransaction() bool {
	return ps.Subquery.NeedsTransaction() || ps.Underlying.NeedsTransaction()
}

var (
	errSqRow    = vterrors.New(vtrpcpb.Code_INVALID_ARGUMENT, "subquery returned more than one row")
	errSqColumn = vterrors.New(vtrpcpb.Code_INVALID_ARGUMENT, "subquery returned more than one column")
)

func (ps *PulloutSubquery) execSubquery(vcursor VCursor, bindVars map[string]*querypb.BindVariable) (map[string]*querypb.BindVariable, error) {
	subqueryBindVars := make(map[string]*querypb.BindVariable, len(bindVars))
	for k, v := range bindVars {
		subqueryBindVars[k] = v
	}
	result, err := vcursor.ExecutePrimitive(ps.Subquery, subqueryBindVars, false)
	if err != nil {
		return nil, err
	}
	combinedVars := make(map[string]*querypb.BindVariable, len(bindVars)+1)
	for k, v := range bindVars {
		combinedVars[k] = v
	}
	switch ps.Opcode {
	case PulloutValue:
		switch len(result.Rows) {
		case 0:
			combinedVars[ps.SubqueryResult] = sqltypes.NullBindVariable
		case 1:
			if len(result.Rows[0]) != 1 {
				return nil, errSqColumn
			}
			combinedVars[ps.SubqueryResult] = sqltypes.ValueBindVariable(result.Rows[0][0])
		default:
			return nil, errSqRow
		}
	case PulloutIn, PulloutNotIn:
		switch len(result.Rows) {
		case 0:
			combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(0)
			// Add a bogus value. It will not be checked.
			combinedVars[ps.SubqueryResult] = &querypb.BindVariable{
				Type:   querypb.Type_TUPLE,
				Values: []*querypb.Value{sqltypes.ValueToProto(sqltypes.NewInt64(0))},
			}
		default:
			if len(result.Rows[0]) != 1 {
				return nil, errSqColumn
			}
			combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(1)
			values := &querypb.BindVariable{
				Type:   querypb.Type_TUPLE,
				Values: make([]*querypb.Value, len(result.Rows)),
			}
			for i, v := range result.Rows {
				values.Values[i] = sqltypes.ValueToProto(v[0])
			}
			combinedVars[ps.SubqueryResult] = values
		}
	case PulloutExists:
		switch len(result.Rows) {
		case 0:
			combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(0)
		default:
			combinedVars[ps.HasValues] = sqltypes.Int64BindVariable(1)
		}
	}
	return combinedVars, nil
}

func (ps *PulloutSubquery) description() PrimitiveDescription {
	other := map[string]interface{}{}
	var pulloutVars []string
	if ps.HasValues != "" {
		pulloutVars = append(pulloutVars, ps.HasValues)
	}
	if ps.SubqueryResult != "" {
		pulloutVars = append(pulloutVars, ps.SubqueryResult)
	}
	if len(pulloutVars) > 0 {
		other["PulloutVars"] = pulloutVars
	}
	return PrimitiveDescription{
		OperatorType: "Subquery",
		Variant:      ps.Opcode.String(),
		Other:        other,
	}
}

// PulloutOpcode is a number representing the opcode
// for the PulloutSubquery primitive.
type PulloutOpcode int

// This is the list of PulloutOpcode values.
const (
	PulloutValue = PulloutOpcode(iota)
	PulloutIn
	PulloutNotIn
	PulloutExists
)

var pulloutName = map[PulloutOpcode]string{
	PulloutValue:  "PulloutValue",
	PulloutIn:     "PulloutIn",
	PulloutNotIn:  "PulloutNotIn",
	PulloutExists: "PulloutExists",
}

func (code PulloutOpcode) String() string {
	return pulloutName[code]
}

// MarshalJSON serializes the PulloutOpcode as a JSON string.
// It's used for testing and diagnostics.
func (code PulloutOpcode) MarshalJSON() ([]byte, error) {
	return ([]byte)(fmt.Sprintf("\"%s\"", code.String())), nil
}

/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package function

import (
	stdErrors "errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

import (
	lru "github.com/hashicorp/golang-lru"

	"github.com/pkg/errors"
)

import (
	"github.com/arana-db/arana/pkg/runtime/ast"
	"github.com/arana-db/arana/pkg/runtime/cmp"
	"github.com/arana-db/arana/pkg/runtime/misc"
)

const _prefixMySQLFunc = "$"

var ErrCannotEvalWithColumnName = stdErrors.New("cannot eval function with column name")

var (
	_globalCalculator     *calculator
	_globalCalculatorOnce sync.Once
)

func globalCalculator() *calculator {
	_globalCalculatorOnce.Do(func() {
		cache, _ := lru.New(1024)
		_globalCalculator = (*calculator)(cache)
	})
	return _globalCalculator
}

func IsEvalWithColumnErr(err error) bool {
	return err == ErrCannotEvalWithColumnName
}

// TranslateFunction translates the given function to internal function name.
func TranslateFunction(name string) string {
	return _prefixMySQLFunc + name
}

func EvalCastFunction(node *ast.CastFunction, args ...interface{}) (interface{}, error) {
	s, err := globalCalculator().build(node)
	if err != nil {
		return nil, err
	}
	return EvalString(s, args...)
}

func EvalCaseWhenFunction(node *ast.CaseWhenElseFunction, args ...interface{}) (interface{}, error) {
	s, err := globalCalculator().build(node)
	if err != nil {
		return nil, err
	}
	return EvalString(s, args...)
}

// EvalFunction calculates the result of math expression with custom args.
func EvalFunction(node *ast.Function, args ...interface{}) (interface{}, error) {
	s, err := globalCalculator().build(node)
	if err != nil {
		return nil, err
	}
	return EvalString(s, args...)
}

// Eval calculates the result of math expression with custom args.
func Eval(node *ast.MathExpressionAtom, args ...interface{}) (interface{}, error) {
	s, err := globalCalculator().build(node)
	if err != nil {
		return nil, err
	}
	return EvalString(s, args...)
}

// EvalString computes the result of given expression script with custom args.
func EvalString(script string, args ...interface{}) (interface{}, error) {
	vm := BorrowVM()
	defer ReturnVM(vm)
	return vm.Eval(script, args)
}

type scriptComputer struct {
	sync.Once
	source interface{}
	script string
	err    error
}

func newScriptComputer(source interface{}) *scriptComputer {
	return &scriptComputer{source: source}
}

func (sc *scriptComputer) compute() (string, error) {
	sc.Do(func() {
		defer func() {
			sc.source = nil
		}()

		switch source := sc.source.(type) {
		case *ast.CaseWhenElseFunction:
			var sb strings.Builder
			if err := caseWhenFunction2script(&sb, source); err != nil {
				sc.err = err
				return
			}
			sc.script = sb.String()
		case *ast.CastFunction:
			var sb strings.Builder
			if err := castFunction2script(&sb, source); err != nil {
				sc.err = err
				return
			}
			sc.script = sb.String()
		case *ast.Function:
			var sb strings.Builder
			if err := function2script(&sb, source); err != nil {
				sc.err = err
				return
			}
			sc.script = sb.String()
		case *ast.MathExpressionAtom:
			var sb strings.Builder
			if err := math2script(&sb, source); err != nil {
				sc.err = err
				return
			}
			sc.script = sb.String()
		default:
			sc.err = errors.Errorf("invalid script source node type %T", source)
		}
	})

	return sc.script, sc.err
}

type calculator lru.Cache

func (c *calculator) cache() *lru.Cache {
	return (*lru.Cache)(c)
}

func (c *calculator) build(node interface{}) (string, error) {
	key := fmt.Sprintf("%T_%p", node, node)

	if exist, ok := c.cache().Get(key); ok {
		return exist.(*scriptComputer).compute()
	}

	newborn := newScriptComputer(node)

	prev, ok, _ := c.cache().PeekOrAdd(key, newborn)
	if ok {
		return prev.(*scriptComputer).compute()
	}

	return newborn.compute()
}

func exprAtom2script(sb *strings.Builder, node ast.ExpressionAtom) error {
	switch v := node.(type) {
	case *ast.IntervalExpressionAtom:
		atom, ok := v.Value.(*ast.AtomPredicateNode)
		if !ok {
			return errors.Errorf("invalid expr %T for interval expression", v.Value)
		}
		if err := exprAtom2script(sb, atom.A); err != nil {
			return errors.WithStack(err)
		}
		sb.WriteString(" * ")
		_, _ = fmt.Fprintf(sb, "%d", v.Duration().Nanoseconds()) // 转换为nano
	case *ast.MathExpressionAtom:
		if err := math2script(sb, v); err != nil {
			return err
		}
	case *ast.ConstantExpressionAtom:
		sb.WriteString(v.String())
	case *ast.UnaryExpressionAtom:
		sb.WriteString(FuncUnary)
		sb.WriteString("('")
		sb.WriteString(v.Operator)
		sb.WriteString("', ")

		switch it := v.Inner.(type) {
		case ast.ExpressionAtom:
			if err := exprAtom2script(sb, it); err != nil {
				return err
			}
		case *ast.BinaryComparisonPredicateNode:
			if err := handleCompareAtom(sb, it.Left); err != nil {
				return err
			}

			sb.WriteByte(' ')

			switch it.Op {
			case cmp.Ceq:
				sb.WriteString("==")
			case cmp.Cne:
				sb.WriteString("!=")
			default:
				_, _ = it.Op.WriteTo(sb)
			}

			sb.WriteByte(' ')
			if err := handleCompareAtom(sb, it.Right); err != nil {
				return err
			}
		default:
			panic("unreachable")
		}

		sb.WriteByte(')')
	case ast.ColumnNameExpressionAtom:
		return ErrCannotEvalWithColumnName
	case *ast.NestedExpressionAtom:
		next := v.First.(*ast.PredicateExpressionNode).P.(*ast.AtomPredicateNode).A
		sb.WriteByte('(')
		if err := exprAtom2script(sb, next); err != nil {
			return err
		}
		sb.WriteByte(')')
	case ast.VariableExpressionAtom:
		writeVariable(sb, v.N())
	case *ast.FunctionCallExpressionAtom:
		switch fn := v.F.(type) {
		case *ast.Function:
			if err := function2script(sb, fn); err != nil {
				return err
			}
		case *ast.AggrFunction:
			return errors.New("aggr function should not appear here")
		case *ast.CastFunction:
			if err := castFunction2script(sb, fn); err != nil {
				return err
			}
		case *ast.CaseWhenElseFunction:
			if err := caseWhenFunction2script(sb, fn); err != nil {
				return err
			}
		default:
			return errors.Errorf("expression atom within function call %T is not supported yet", fn)
		}
	default:
		return errors.Errorf("expression atom within %T is not supported yet", v)
	}
	return nil
}

func math2script(sb *strings.Builder, node *ast.MathExpressionAtom) error {
	if err := exprAtom2script(sb, node.Left); err != nil {
		return err
	}

	sb.WriteByte(' ')
	sb.WriteString(node.Operator)
	sb.WriteByte(' ')

	if err := exprAtom2script(sb, node.Right); err != nil {
		return err
	}

	return nil
}

func writeVariable(sb *strings.Builder, n int) {
	sb.WriteString("arguments[")
	sb.WriteString(strconv.FormatInt(int64(n), 10))
	sb.WriteByte(']')
}

func castFunction2script(sb *strings.Builder, node *ast.CastFunction) error {
	if cast, ok := node.GetCast(); ok {
		switch cast.Type() {
		case ast.CastToUnsigned, ast.CastToUnsignedInteger:
			writeFuncName(sb, "CAST_UNSIGNED")
			sb.WriteByte('(')
		case ast.CastToSigned, ast.CastToSignedInteger:
			writeFuncName(sb, "CAST_SIGNED")
			sb.WriteByte('(')
		case ast.CastToBinary:
			// TODO: 支持binary
			return errors.New("cast to binary is not supported yet")
		case ast.CastToNChar:
			writeFuncName(sb, "CAST_NCHAR")
			sb.WriteByte('(')
			if d, _ := cast.Dimensions(); d > 0 {
				sb.WriteString(strconv.FormatInt(d, 10))
			} else {
				sb.WriteByte('0')
			}
			sb.WriteString(", ")
		case ast.CastToChar:
			writeFuncName(sb, "CAST_CHAR")
			sb.WriteByte('(')
			if d, _ := cast.Dimensions(); d > 0 {
				sb.WriteString(strconv.FormatInt(d, 10))
			} else {
				sb.WriteByte('0')
			}

			sb.WriteString(", ")

			if cs, ok := cast.Charset(); ok {
				sb.WriteByte('\'')
				sb.WriteString(misc.Escape(cs, misc.EscapeSingleQuote))
				sb.WriteByte('\'')
			} else {
				sb.WriteString("''")
			}

			sb.WriteString(", ")
		case ast.CastToDate:
			writeFuncName(sb, "CAST_DATE")
			sb.WriteByte('(')
		case ast.CastToDateTime:
			writeFuncName(sb, "CAST_DATETIME")
			sb.WriteByte('(')
		case ast.CastToTime:
			writeFuncName(sb, "CAST_TIME")
			sb.WriteByte('(')
		case ast.CastToJson:
			// TODO: support cast json
			return errors.New("cast to json is not supported yet")
		case ast.CastToDecimal:
			writeFuncName(sb, "CAST_DECIMAL")
			sb.WriteByte('(')
			d0, d1 := cast.Dimensions()
			if d0 > 0 {
				sb.WriteString(strconv.FormatInt(d0, 10))
			} else {
				sb.WriteByte('0')
			}
			sb.WriteString(", ")

			if d1 > 0 {
				sb.WriteString(strconv.FormatInt(d1, 10))
			} else {
				sb.WriteByte('0')
			}
			sb.WriteString(", ")
		}
	} else if charset, ok := node.GetCharset(); ok {
		writeFuncName(sb, "CAST_CHARSET(")

		sb.WriteByte('\'')
		sb.WriteString(misc.Escape(charset, misc.EscapeSingleQuote))
		sb.WriteByte('\'')

		sb.WriteString(", ")
	} else {
		panic("unreachable")
	}

	next := node.Source().(*ast.PredicateExpressionNode).P.(*ast.AtomPredicateNode).A
	if err := exprAtom2script(sb, next); err != nil {
		return err
	}

	sb.WriteByte(')')

	return nil
}

func function2script(sb *strings.Builder, node *ast.Function) error {
	writeFuncName(sb, node.Name())
	sb.WriteByte('(')
	for i, arg := range node.Args() {
		if i > 0 {
			sb.WriteByte(',')
			sb.WriteByte(' ')
		}
		if err := handleArg(sb, arg); err != nil {
			return err
		}
	}
	sb.WriteByte(')')
	return nil
}

func handleCompareAtom(sb *strings.Builder, node ast.PredicateNode) error {
	switch l := node.(type) {
	case *ast.AtomPredicateNode:
		if err := exprAtom2script(sb, l.A); err != nil {
			return err
		}
	default:
		return errors.Errorf("unsupported compare atom node %T in case-when function", l)
	}
	return nil
}

func handleArg(sb *strings.Builder, arg *ast.FunctionArg) error {
	switch arg.Type {
	case ast.FunctionArgColumn:
		return ErrCannotEvalWithColumnName
	case ast.FunctionArgConstant:
		_ = arg.Restore(ast.RestoreDefault, sb, nil)
	case ast.FunctionArgExpression:
		pn := arg.Value.(*ast.PredicateExpressionNode).P
		switch p := pn.(type) {
		case *ast.AtomPredicateNode:
			next := p.A
			if err := exprAtom2script(sb, next); err != nil {
				return err
			}
		case *ast.BinaryComparisonPredicateNode:
			if err := handleCompareAtom(sb, p.Left); err != nil {
				return err
			}

			sb.WriteByte(' ')

			switch p.Op {
			case cmp.Ceq:
				sb.WriteString("==")
			case cmp.Cne:
				sb.WriteString("!=")
			default:
				_, _ = p.Op.WriteTo(sb)
			}

			sb.WriteByte(' ')
			if err := handleCompareAtom(sb, p.Right); err != nil {
				return err
			}
		default:
			return errors.Errorf("unsupported %T", p)
		}

	case ast.FunctionArgFunction:
		if err := function2script(sb, arg.Value.(*ast.Function)); err != nil {
			return err
		}
	case ast.FunctionArgCastFunction:
		if err := castFunction2script(sb, arg.Value.(*ast.CastFunction)); err != nil {
			return err
		}
	case ast.FunctionArgCaseWhenElseFunction:
		if err := caseWhenFunction2script(sb, arg.Value.(*ast.CaseWhenElseFunction)); err != nil {
			return err
		}
	}

	return nil
}

func caseWhenFunction2script(sb *strings.Builder, node *ast.CaseWhenElseFunction) error {
	var caseScript string

	// convert CASE header to script
	// eg: CASE 2+1 WHEN 1 THEN 'A' WHEN 2 THEN 'B' WHEN 3 THEN 'C' ELSE '*' END
	// will be converted to: $IF(1 == (2+1), 'A', $IF(2 == (2+1), 'B', $IF(3 == (2+1), 'C', '*' )))
	if c := node.Case(); c != nil {
		var b strings.Builder
		switch v := c.(type) {
		case *ast.PredicateExpressionNode:
			switch p := v.P.(type) {
			case *ast.AtomPredicateNode:
				if err := exprAtom2script(&b, p.A); err != nil {
					return err
				}
			default:
				return errors.Errorf("invalid expression type %T as the CASE body", v)
			}
		default:
			return errors.Errorf("invalid expression type %T as the CASE body", v)
		}
		caseScript = b.String()
	}

	for i, branch := range node.Branches() {
		var (
			when = branch[0]
			then = branch[1]
		)

		if i > 0 {
			sb.WriteString(", ")
		}

		writeFuncName(sb, "IF")

		sb.WriteByte('(')

		if err := handleArg(sb, when); err != nil {
			return err
		}

		// 写入CASE头
		if len(caseScript) > 0 {
			sb.WriteString(" == (")
			sb.WriteString(caseScript)
			sb.WriteByte(')')
		}

		sb.WriteString(", ")

		if err := handleArg(sb, then); err != nil {
			return err
		}
	}

	sb.WriteString(", ")

	if els, ok := node.Else(); ok {
		if err := handleArg(sb, els); err != nil {
			return err
		}
	} else {
		sb.WriteString("null")
	}

	for i := 0; i < len(node.Branches()); i++ {
		sb.WriteByte(')')
	}

	return nil
}

func writeFuncName(sb *strings.Builder, name string) {
	sb.WriteString(_prefixMySQLFunc)
	sb.WriteString(name)
}

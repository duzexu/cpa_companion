package companion

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"
)

// A billing expression is a deliberately small subset of New API's syntax.
// It cannot call Go functions, read files, or access request data. Unsupported
// variables (audio, images, cache-write duration and request parameters) are
// rejected because the proxy does not collect their usage yet.
type billingExpression struct {
	condition ast.Expr
	thenExpr  *billingExpression
	elseExpr  *billingExpression
	value     ast.Expr
}

func parseBillingExpression(raw string) (*billingExpression, error) {
	if len(raw) == 0 || len(raw) > 1024 {
		return nil, errors.New("expression length must be 1–1024 characters")
	}
	return parseBillingPart(strings.TrimSpace(raw), 0)
}

func parseBillingPart(raw string, depth int) (*billingExpression, error) {
	if depth > 8 {
		return nil, errors.New("expression nesting is too deep")
	}
	question, colon := billingTernary(raw)
	if question >= 0 {
		if colon < 0 {
			return nil, errors.New("conditional expression is missing ':'")
		}
		condition, err := parser.ParseExpr(strings.TrimSpace(raw[:question]))
		if err != nil {
			return nil, err
		}
		if err := validateBillingAST(condition); err != nil {
			return nil, err
		}
		thenExpr, err := parseBillingPart(strings.TrimSpace(raw[question+1:colon]), depth+1)
		if err != nil {
			return nil, err
		}
		elseExpr, err := parseBillingPart(strings.TrimSpace(raw[colon+1:]), depth+1)
		if err != nil {
			return nil, err
		}
		return &billingExpression{condition: condition, thenExpr: thenExpr, elseExpr: elseExpr}, nil
	}
	value, err := parser.ParseExpr(raw)
	if err != nil {
		return nil, err
	}
	if err := validateBillingAST(value); err != nil {
		return nil, err
	}
	return &billingExpression{value: value}, nil
}

func billingTernary(raw string) (int, int) {
	depth, question, quote := 0, -1, false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			if i == 0 || raw[i-1] != '\\' {
				quote = !quote
			}
		case '(':
			if !quote {
				depth++
			}
		case ')':
			if !quote {
				depth--
			}
		case '?':
			if !quote && depth == 0 && question < 0 {
				question = i
			}
		case ':':
			if !quote && depth == 0 && question >= 0 {
				return question, i
			}
		}
	}
	return question, -1
}

func validateBillingAST(expr ast.Expr) error {
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return validateBillingAST(node.X)
	case *ast.BasicLit:
		if node.Kind == token.INT || node.Kind == token.FLOAT || node.Kind == token.STRING {
			return nil
		}
	case *ast.Ident:
		switch node.Name {
		case "p", "c", "cr", "cc", "len", "true", "false":
			return nil
		}
	case *ast.UnaryExpr:
		if node.Op == token.ADD || node.Op == token.SUB || node.Op == token.NOT {
			return validateBillingAST(node.X)
		}
	case *ast.BinaryExpr:
		switch node.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.LSS, token.LEQ, token.GTR, token.GEQ, token.EQL, token.NEQ, token.LAND, token.LOR:
			if err := validateBillingAST(node.X); err != nil {
				return err
			}
			return validateBillingAST(node.Y)
		}
	case *ast.CallExpr:
		name, ok := node.Fun.(*ast.Ident)
		if !ok || name.Name != "tier" || len(node.Args) != 2 {
			break
		}
		label, ok := node.Args[0].(*ast.BasicLit)
		if !ok || label.Kind != token.STRING {
			break
		}
		return validateBillingAST(node.Args[1])
	}
	return fmt.Errorf("unsupported billing expression element: %T", expr)
}

func chargeExpression(raw string, usage tokenUsage) (int64, error) {
	compiled, err := parseBillingExpression(raw)
	if err != nil {
		return 0, err
	}
	values := map[string]float64{
		"p":   float64(usage.Input-usage.CacheRead-usage.CacheWrite) / 1_000_000,
		"c":   float64(usage.Output) / 1_000_000,
		"cr":  float64(usage.CacheRead) / 1_000_000,
		"cc":  float64(usage.CacheWrite) / 1_000_000,
		"len": float64(usage.Input),
	}
	result, err := compiled.eval(values)
	if err != nil {
		return 0, err
	}
	amount, ok := result.(float64)
	if !ok || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 || amount > float64(math.MaxInt64)/float64(picoUSD) {
		return 0, errors.New("billing expression returned an invalid charge")
	}
	return int64(math.Round(amount * float64(picoUSD))), nil
}

func validatePriceExpression(raw string) error {
	if _, err := parseBillingExpression(raw); err != nil {
		return err
	}
	for _, size := range []int64{0, 272000, 512001} {
		if _, err := chargeExpression(raw, tokenUsage{Input: size, Output: 1000}); err != nil {
			return err
		}
	}
	return nil
}

func (e *billingExpression) eval(values map[string]float64) (any, error) {
	if e.condition != nil {
		v, err := evalBillingAST(e.condition, values)
		if err != nil {
			return nil, err
		}
		condition, ok := v.(bool)
		if !ok {
			return nil, errors.New("billing condition must be boolean")
		}
		if condition {
			return e.thenExpr.eval(values)
		}
		return e.elseExpr.eval(values)
	}
	return evalBillingAST(e.value, values)
}

func evalBillingAST(expr ast.Expr, values map[string]float64) (any, error) {
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return evalBillingAST(node.X, values)
	case *ast.BasicLit:
		if node.Kind == token.STRING {
			return strconv.Unquote(node.Value)
		}
		return strconv.ParseFloat(node.Value, 64)
	case *ast.Ident:
		if node.Name == "true" {
			return true, nil
		}
		if node.Name == "false" {
			return false, nil
		}
		return values[node.Name], nil
	case *ast.CallExpr:
		return evalBillingAST(node.Args[1], values)
	case *ast.UnaryExpr:
		v, err := evalBillingAST(node.X, values)
		if err != nil {
			return nil, err
		}
		if node.Op == token.NOT {
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("invalid boolean operand")
			}
			return !b, nil
		}
		n, ok := v.(float64)
		if !ok {
			return nil, errors.New("invalid numeric operand")
		}
		if node.Op == token.SUB {
			return -n, nil
		}
		return n, nil
	case *ast.BinaryExpr:
		left, err := evalBillingAST(node.X, values)
		if err != nil {
			return nil, err
		}
		if node.Op == token.LAND || node.Op == token.LOR {
			b, ok := left.(bool)
			if !ok {
				return nil, errors.New("invalid boolean operand")
			}
			if node.Op == token.LAND && !b {
				return false, nil
			}
			if node.Op == token.LOR && b {
				return true, nil
			}
			right, err := evalBillingAST(node.Y, values)
			if err != nil {
				return nil, err
			}
			r, ok := right.(bool)
			if !ok {
				return nil, errors.New("invalid boolean operand")
			}
			return r, nil
		}
		right, err := evalBillingAST(node.Y, values)
		if err != nil {
			return nil, err
		}
		x, xok := left.(float64)
		y, yok := right.(float64)
		if !xok || !yok {
			return nil, errors.New("billing operands must be numeric")
		}
		switch node.Op {
		case token.ADD:
			return x + y, nil
		case token.SUB:
			return x - y, nil
		case token.MUL:
			return x * y, nil
		case token.QUO:
			if y == 0 {
				return nil, errors.New("division by zero")
			}
			return x / y, nil
		case token.LSS:
			return x < y, nil
		case token.LEQ:
			return x <= y, nil
		case token.GTR:
			return x > y, nil
		case token.GEQ:
			return x >= y, nil
		case token.EQL:
			return x == y, nil
		case token.NEQ:
			return x != y, nil
		}
	}
	return nil, errors.New("invalid billing expression")
}

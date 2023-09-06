package pg

// Framework code is generated by the generator.

import (
	"fmt"
	"log/slog"

	"github.com/pkg/errors"

	"github.com/bytebase/bytebase/backend/plugin/advisor"
	"github.com/bytebase/bytebase/backend/plugin/advisor/db"
	parser "github.com/bytebase/bytebase/backend/plugin/parser/sql"
	"github.com/bytebase/bytebase/backend/plugin/parser/sql/ast"
)

var (
	_ advisor.Advisor = (*IndexPrimaryKeyTypeAllowlistAdvisor)(nil)
	_ ast.Visitor     = (*indexPrimaryKeyTypeAllowlistChecker)(nil)
)

func init() {
	advisor.Register(db.Postgres, advisor.PostgreSQLPrimaryKeyTypeAllowlist, &IndexPrimaryKeyTypeAllowlistAdvisor{})
}

// IndexPrimaryKeyTypeAllowlistAdvisor is the advisor checking for primary key type allowlist.
type IndexPrimaryKeyTypeAllowlistAdvisor struct {
}

// Check checks for primary key type allowlist.
func (*IndexPrimaryKeyTypeAllowlistAdvisor) Check(ctx advisor.Context, _ string) ([]advisor.Advice, error) {
	stmtList, ok := ctx.AST.([]ast.Node)
	if !ok {
		return nil, errors.Errorf("failed to convert to Node")
	}

	level, err := advisor.NewStatusBySQLReviewRuleLevel(ctx.Rule.Level)
	if err != nil {
		return nil, err
	}
	payload, err := advisor.UnmarshalStringArrayTypeRulePayload(ctx.Rule.Payload)
	if err != nil {
		return nil, err
	}
	checker := &indexPrimaryKeyTypeAllowlistChecker{
		level:     level,
		title:     string(ctx.Rule.Type),
		allowlist: payload.List,
	}

	for _, stmt := range stmtList {
		ast.Walk(checker, stmt)
	}

	if len(checker.adviceList) == 0 {
		checker.adviceList = append(checker.adviceList, advisor.Advice{
			Status:  advisor.Success,
			Code:    advisor.Ok,
			Title:   "OK",
			Content: "",
		})
	}
	return checker.adviceList, nil
}

type indexPrimaryKeyTypeAllowlistChecker struct {
	adviceList []advisor.Advice
	level      advisor.Status
	title      string
	allowlist  []string
}

// Visit implements ast.Visitor interface.
func (checker *indexPrimaryKeyTypeAllowlistChecker) Visit(in ast.Node) ast.Visitor {
	var columnList []*ast.ColumnDef
	columnMap := make(map[string]*ast.ColumnDef)
	if node, ok := in.(*ast.CreateTableStmt); ok {
		for _, column := range node.ColumnList {
			columnMap[column.ColumnName] = column
			if isPKColumn(column) {
				columnList = append(columnList, column)
			}
		}
		for _, constraint := range node.ConstraintList {
			if constraint.Type == ast.ConstraintTypePrimary {
				for _, key := range constraint.KeyList {
					if column, exists := columnMap[key]; exists {
						columnList = append(columnList, column)
					}
				}
			}
		}
	}

	for _, column := range columnList {
		if !allowType(checker.allowlist, column.Type) {
			typeText, err := parser.Deparse(parser.Postgres, parser.DeparseContext{}, column.Type)
			if err != nil {
				slog.Warn("Failed to deparse the PostgreSQL data type",
					slog.String("columnName", column.ColumnName),
					slog.String("originalSQL", in.Text()))
				typeText = ""
			}
			checker.adviceList = append(checker.adviceList, advisor.Advice{
				Status:  checker.level,
				Code:    advisor.IndexPKType,
				Title:   checker.title,
				Content: fmt.Sprintf(`The column "%s" is one of the primary key, but its type "%s" is not in allowlist`, column.ColumnName, typeText),
				Line:    column.LastLine(),
			})
		}
	}

	return checker
}

func allowType(allowlist []string, columnType ast.DataType) bool {
	for _, tp := range allowlist {
		if columnType.EquivalentType(tp) {
			return true
		}
	}
	return false
}

func isPKColumn(column *ast.ColumnDef) bool {
	for _, constraint := range column.ConstraintList {
		if constraint.Type == ast.ConstraintTypePrimary {
			return true
		}
	}

	return false
}

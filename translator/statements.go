package translator

import (
	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"fmt"
	"go/ast"
	"go/token"
	"strings"
)

func (c *PkgContext) translateStmtList(stmts []ast.Stmt) {
	for _, stmt := range stmts {
		c.translateStmt(stmt, "")
	}
}

func (c *PkgContext) translateStmt(stmt ast.Stmt, label string) {
	switch s := stmt.(type) {
	case *ast.BlockStmt:
		c.Printf("{")
		c.Indent(func() {
			c.translateStmtList(s.List)
		})
		c.Printf("}")

	case *ast.IfStmt:
		c.translateStmt(s.Init, "")
		var caseClauses []ast.Stmt
		ifStmt := s
		for {
			caseClauses = append(caseClauses, &ast.CaseClause{List: []ast.Expr{ifStmt.Cond}, Body: ifStmt.Body.List})
			switch elseStmt := ifStmt.Else.(type) {
			case *ast.IfStmt:
				ifStmt = elseStmt
				continue
			case *ast.BlockStmt:
				caseClauses = append(caseClauses, &ast.CaseClause{List: nil, Body: elseStmt.List})
			case *ast.EmptyStmt, nil:
				// no else clause
			default:
				panic(fmt.Sprintf("Unhandled else: %T\n", elseStmt))
			}
			break
		}
		c.translateBranchingStmt(caseClauses, false, c.translateExpr, nil, label)

	case *ast.SwitchStmt:
		c.translateStmt(s.Init, "")
		translateCond := func(cond ast.Expr) string {
			return c.translateExpr(cond)
		}
		if s.Tag != nil {
			refVar := c.newVariable("_ref")
			c.Printf("%s = %s;", refVar, c.translateExpr(s.Tag))
			translateCond = func(cond ast.Expr) string {
				refId := ast.NewIdent(refVar)
				c.info.Types[refId] = c.info.Types[s.Tag]
				return c.translateExpr(&ast.BinaryExpr{
					X:  refId,
					Op: token.EQL,
					Y:  cond,
				})
			}
		}
		c.translateBranchingStmt(s.Body.List, true, translateCond, nil, label)

	case *ast.TypeSwitchStmt:
		c.translateStmt(s.Init, "")
		var expr ast.Expr
		var typeSwitchVar string
		switch a := s.Assign.(type) {
		case *ast.AssignStmt:
			expr = a.Rhs[0].(*ast.TypeAssertExpr).X
			typeSwitchVar = c.newVariable(a.Lhs[0].(*ast.Ident).Name)
			for _, caseClause := range s.Body.List {
				c.objectVars[c.info.Implicits[caseClause]] = typeSwitchVar
			}
		case *ast.ExprStmt:
			expr = a.X.(*ast.TypeAssertExpr).X
		}
		refVar := c.newVariable("_ref")
		typeVar := c.newVariable("_type")
		c.Printf("%s = %s;", refVar, c.translateExpr(expr))
		c.Printf("%s = %s !== null ? %s.constructor : null;", typeVar, refVar, refVar)
		translateCond := func(cond ast.Expr) string {
			return c.typeCheck(typeVar, c.info.Types[cond])
		}
		printCaseBodyPrefix := func(conds []ast.Expr) {
			if typeSwitchVar == "" {
				return
			}
			value := refVar
			if len(conds) == 1 {
				t := c.info.Types[conds[0]]
				if _, isInterface := t.Underlying().(*types.Interface); !isInterface && !types.IsIdentical(t, types.Typ[types.UntypedNil]) {
					value += ".Go$val"
				}
			}
			c.Printf("%s = %s;", typeSwitchVar, value)
		}
		c.translateBranchingStmt(s.Body.List, true, translateCond, printCaseBodyPrefix, label)

	case *ast.ForStmt:
		c.translateStmt(s.Init, "")
		cond := "true"
		if s.Cond != nil {
			cond = c.translateExpr(s.Cond)
		}
		p := c.postLoopStmt[""]
		defer func() {
			delete(c.postLoopStmt, label)
			c.postLoopStmt[""] = p
		}()
		c.postLoopStmt[""] = s.Post
		c.postLoopStmt[label] = s.Post
		c.Printf("%swhile (%s) {", label, cond)
		c.Indent(func() {
			c.translateStmtList(s.Body.List)
			c.translateStmt(s.Post, "")
		})
		c.Printf("}")

	case *ast.RangeStmt:
		p := c.postLoopStmt[""]
		defer func() { c.postLoopStmt[""] = p }()
		delete(c.postLoopStmt, "")

		refVar := c.newVariable("_ref")
		c.Printf("%s = %s;", refVar, c.translateExpr(s.X))

		iVar := c.newVariable("_i")
		c.Printf("%s = 0;", iVar)

		switch t := c.info.Types[s.X].Underlying().(type) {
		case *types.Basic:
			runeVar := c.newVariable("_rune")
			c.Printf("%sfor (; %s < %s.length; %s += %s[1]) {", label, iVar, refVar, iVar, runeVar)
			c.Indent(func() {
				c.Printf("%s = Go$decodeRune(%s, %s);", runeVar, refVar, iVar)
				c.translateAssign(s.Value, runeVar+"[0]")
				c.translateAssign(s.Key, iVar)
				c.translateStmtList(s.Body.List)
			})
			c.Printf("}")

		case *types.Map:
			keysVar := c.newVariable("_keys")
			c.Printf("%s = %s !== null ? Go$keys(%s) : [];", keysVar, refVar, refVar)
			c.Printf("%sfor (; %s < %s.length; %s++) {", label, iVar, keysVar, iVar)
			c.Indent(func() {
				entryVar := c.newVariable("_entry")
				c.Printf("%s = %s[%s[%s]];", entryVar, refVar, keysVar, iVar)
				c.translateAssign(s.Value, entryVar+".v")
				c.translateAssign(s.Key, entryVar+".k")
				c.translateStmtList(s.Body.List)
			})
			c.Printf("}")

		case *types.Array, *types.Pointer, *types.Slice:
			var length string
			switch t2 := t.(type) {
			case *types.Array:
				length = fmt.Sprintf("%d", t2.Len())
			case *types.Pointer:
				length = fmt.Sprintf("%d", t2.Elem().(*types.Array).Len())
			case *types.Slice:
				length = refVar + ".length"
			}
			c.Printf("%sfor (; %s < %s; %s++) {", label, iVar, length, iVar)
			c.Indent(func() {
				if s.Value != nil && !isUnderscore(s.Value) {
					x := ast.NewIdent(refVar)
					index := ast.NewIdent(iVar)
					indexExpr := &ast.IndexExpr{X: x, Index: index}
					c.info.Types[x] = t
					c.info.Types[index] = types.Typ[types.Int]
					et := elemType(t)
					c.info.Types[indexExpr] = et
					c.translateAssign(s.Value, c.translateExprToType(indexExpr, et))
				}
				c.translateAssign(s.Key, iVar)
				c.translateStmtList(s.Body.List)
			})
			c.Printf("}")

		case *types.Chan:
			c.Printf(`throw new Go$Panic("Channels not supported");`)

		default:
			panic("")
		}

	case *ast.BranchStmt:
		label := ""
		postLoopStmt := c.postLoopStmt[""]
		if s.Label != nil {
			label = " " + s.Label.Name
			postLoopStmt = c.postLoopStmt[s.Label.Name+": "]
		}
		switch s.Tok {
		case token.BREAK:
			c.Printf("break%s;", label)
		case token.CONTINUE:
			c.translateStmt(postLoopStmt, "")
			c.Printf("continue%s;", label)
		case token.GOTO:
			c.Printf(`throw new Go$Panic("Statement not supported: goto");`)
		case token.FALLTHROUGH:
			// handled in CaseClause
		default:
			panic("Unhandled branch statment: " + s.Tok.String())
		}

	case *ast.ReturnStmt:
		results := s.Results
		if c.resultNames != nil {
			if len(s.Results) != 0 {
				c.translateStmt(&ast.AssignStmt{
					Lhs: c.resultNames,
					Tok: token.ASSIGN,
					Rhs: s.Results,
				}, label)
			}
			results = c.resultNames
		}
		switch len(results) {
		case 0:
			c.Printf("return;")
		case 1:
			if c.functionSig.Results().Len() > 1 {
				c.Printf("return %s;", c.translateExpr(results[0]))
				return
			}
			c.Printf("return %s;", c.translateExprToType(results[0], c.functionSig.Results().At(0).Type()))
		default:
			values := make([]string, len(results))
			for i, result := range results {
				values[i] = c.translateExprToType(result, c.functionSig.Results().At(i).Type())
			}
			c.Printf("return [%s];", strings.Join(values, ", "))
		}

	case *ast.DeferStmt:
		if ident, isIdent := s.Call.Fun.(*ast.Ident); isIdent {
			if builtin, isBuiltin := c.info.Objects[ident].(*types.Builtin); isBuiltin {
				if builtin.Name() == "recover" {
					c.Printf("Go$deferred.push({ fun: Go$recover, args: [] });")
					return
				}
				args := make([]ast.Expr, len(s.Call.Args))
				for i, arg := range s.Call.Args {
					argIdent := ast.NewIdent(c.newVariable("_arg"))
					c.info.Types[argIdent] = c.info.Types[arg]
					args[i] = argIdent
				}
				call := c.translateExpr(&ast.CallExpr{
					Fun:      s.Call.Fun,
					Args:     args,
					Ellipsis: s.Call.Ellipsis,
				})
				c.Printf("Go$deferred.push({ fun: function(%s) { %s; }, args: [%s] });", strings.Join(c.translateExprSlice(args, nil), ", "), call, strings.Join(c.translateExprSlice(s.Call.Args, nil), ", "))
				return
			}
		}
		sig := c.info.Types[s.Call.Fun].Underlying().(*types.Signature)
		args := c.translateArgs(sig, s.Call.Args, s.Call.Ellipsis.IsValid())
		if sel, isSelector := s.Call.Fun.(*ast.SelectorExpr); isSelector {
			c.Printf(`Go$deferred.push({ recv: %s, method: "%s", args: [%s] });`, c.translateExpr(sel.X), sel.Sel.Name, args)
			return
		}
		c.Printf("Go$deferred.push({ fun: %s, args: [%s] });", c.translateExpr(s.Call.Fun), args)

	case *ast.ExprStmt:
		c.Printf("%s;", c.translateExpr(s.X))

	case *ast.DeclStmt:
		for _, spec := range s.Decl.(*ast.GenDecl).Specs {
			c.translateSpec(spec)
		}

	case *ast.LabeledStmt:
		c.translateStmt(s.Stmt, s.Label.Name+": ")

	case *ast.AssignStmt:
		if s.Tok != token.ASSIGN && s.Tok != token.DEFINE {
			var op token.Token
			switch s.Tok {
			case token.ADD_ASSIGN:
				op = token.ADD
			case token.SUB_ASSIGN:
				op = token.SUB
			case token.MUL_ASSIGN:
				op = token.MUL
			case token.QUO_ASSIGN:
				op = token.QUO
			case token.REM_ASSIGN:
				op = token.REM
			case token.AND_ASSIGN:
				op = token.AND
			case token.OR_ASSIGN:
				op = token.OR
			case token.XOR_ASSIGN:
				op = token.XOR
			case token.SHL_ASSIGN:
				op = token.SHL
			case token.SHR_ASSIGN:
				op = token.SHR
			case token.AND_NOT_ASSIGN:
				op = token.AND_NOT
			default:
				panic(s.Tok)
			}
			parenExpr := &ast.ParenExpr{
				X: s.Rhs[0],
			}
			c.info.Types[parenExpr] = c.info.Types[s.Rhs[0]]
			binaryExpr := &ast.BinaryExpr{
				X:  s.Lhs[0],
				Op: op,
				Y:  parenExpr,
			}
			c.info.Types[binaryExpr] = c.info.Types[s.Lhs[0]]
			c.translateAssign(s.Lhs[0], c.translateExpr(binaryExpr))
			return
		}

		if s.Tok == token.DEFINE {
			for _, lhs := range s.Lhs {
				if !isUnderscore(lhs) {
					c.info.Types[lhs] = c.info.Objects[lhs.(*ast.Ident)].Type()
				}
			}
		}

		rhss := make([]string, len(s.Lhs))

		switch {
		case len(s.Lhs) == 1 && len(s.Rhs) == 1:
			rhss[0] = c.translateExprToType(s.Rhs[0], c.info.Types[s.Lhs[0]])
			if isUnderscore(s.Lhs[0]) {
				c.Printf("%s;", rhss[0])
				return
			}

		case len(s.Lhs) > 1 && len(s.Rhs) == 1:
			tuple := c.info.Types[s.Rhs[0]].(*types.Tuple)
			for i := range s.Lhs {
				id := ast.NewIdent(fmt.Sprintf("Go$tuple[%d]", i))
				c.info.Types[id] = tuple.At(i).Type()
				rhss[i] = c.translateExprToType(id, c.info.Types[s.Lhs[i]])
			}
			c.Printf("Go$tuple = %s;", c.translateExpr(s.Rhs[0]))

		case len(s.Lhs) == len(s.Rhs):
			parts := make([]string, len(s.Rhs))
			for i, rhs := range s.Rhs {
				parts[i] = c.translateExprToType(rhs, c.info.Types[s.Lhs[i]])
				rhss[i] = fmt.Sprintf("Go$tuple[%d]", i)
			}
			c.Printf("Go$tuple = [%s];", strings.Join(parts, ", "))

		default:
			panic("Invalid arity of AssignStmt.")

		}

		for i, lhs := range s.Lhs {
			c.translateAssign(lhs, rhss[i])
		}

	case *ast.IncDecStmt:
		t := c.info.Types[s.X]
		if iExpr, isIExpr := s.X.(*ast.IndexExpr); isIExpr {
			switch u := c.info.Types[iExpr.X].Underlying().(type) {
			case *types.Array:
				t = u.Elem()
			case *types.Slice:
				t = u.Elem()
			case *types.Map:
				t = u.Elem()
			}
		}

		tok := token.ADD_ASSIGN
		if s.Tok == token.DEC {
			tok = token.SUB_ASSIGN
		}
		one := &ast.BasicLit{
			Kind:  token.INT,
			Value: "1",
		}
		c.info.Types[one] = t
		c.info.Values[one] = exact.MakeInt64(1)
		c.translateStmt(&ast.AssignStmt{
			Lhs: []ast.Expr{s.X},
			Tok: tok,
			Rhs: []ast.Expr{one},
		}, label)

	case *ast.SelectStmt:
		c.Printf(`throw new Go$Panic("Statement not supported: select");`)

	case *ast.GoStmt:
		c.Printf(`throw new Go$Panic("Statement not supported: go");`)

	case *ast.SendStmt:
		c.Printf(`throw new Go$Panic("Statement not supported: send");`)

	case *ast.EmptyStmt, nil:
		// skip

	default:
		panic(fmt.Sprintf("Unhandled statement: %T\n", s))

	}
}

func (c *PkgContext) translateBranchingStmt(caseClauses []ast.Stmt, isSwitch bool, translateCond func(ast.Expr) string, printCaseBodyPrefix func([]ast.Expr), label string) {
	if len(caseClauses) == 0 {
		return
	}
	if len(caseClauses) == 1 && caseClauses[0].(*ast.CaseClause).List == nil {
		c.translateStmtList(caseClauses[0].(*ast.CaseClause).Body)
		return
	}

	clauseStmts := make([][]ast.Stmt, len(caseClauses))
	openClauses := make([]int, 0)
	for i, child := range caseClauses {
		caseClause := child.(*ast.CaseClause)
		openClauses = append(openClauses, i)
		for _, j := range openClauses {
			clauseStmts[j] = append(clauseStmts[j], caseClause.Body...)
		}
		if !hasFallthrough(caseClause) {
			openClauses = nil
		}
	}

	printBody := func() {
		var defaultClause []ast.Stmt
		elsePrefix := ""
		for i, child := range caseClauses {
			caseClause := child.(*ast.CaseClause)
			if len(caseClause.List) == 0 {
				defaultClause = clauseStmts[i]
				if defaultClause == nil {
					defaultClause = []ast.Stmt{}
				}
				continue
			}
			conds := make([]string, len(caseClause.List))
			for i, cond := range caseClause.List {
				conds[i] = translateCond(cond)
			}
			c.Printf("%sif (%s) {", elsePrefix, strings.Join(conds, " || "))
			c.Indent(func() {
				if printCaseBodyPrefix != nil {
					printCaseBodyPrefix(caseClause.List)
				}
				c.translateStmtList(clauseStmts[i])
			})
			elsePrefix = "} else "
		}
		if defaultClause != nil {
			c.Printf("} else {")
			c.Indent(func() {
				if printCaseBodyPrefix != nil {
					printCaseBodyPrefix(nil)
				}
				c.translateStmtList(defaultClause)
			})
		}
		c.Printf("}")
	}

	if !isSwitch {
		printBody()
		return
	}

	v := HasBreakVisitor{}
	for _, child := range caseClauses {
		ast.Walk(&v, child)
	}
	if !v.hasBreak && label == "" {
		printBody()
		return
	}

	c.Printf("%sswitch (undefined) {", label)
	c.Printf("default:")
	c.Indent(func() {
		printBody()
	})
	c.Printf("}")
}

func (c *PkgContext) translateAssign(lhs ast.Expr, rhs string) {
	for {
		if p, isParen := lhs.(*ast.ParenExpr); isParen {
			lhs = p.X
			continue
		}
		break
	}
	if lhs == nil || isUnderscore(lhs) {
		return
	}

	for {
		if p, isParenExpr := lhs.(*ast.ParenExpr); isParenExpr {
			lhs = p.X
			continue
		}
		break
	}

	switch l := lhs.(type) {
	case *ast.Ident:
		c.Printf("%s = %s;", c.objectName(c.info.Objects[l]), rhs)
	case *ast.SelectorExpr:
		if structLhs, isStruct := c.info.Types[lhs].Underlying().(*types.Struct); isStruct {
			c.Printf("Go$obj = %s;", rhs)
			s := c.translateExpr(l)
			for i := 0; i < structLhs.NumFields(); i++ {
				field := structLhs.Field(i)
				c.Printf("%s.%s = Go$obj.%s;", s, field.Name(), field.Name())
			}
			return
		}
		c.Printf("%s = %s;", c.translateExpr(l), rhs)
	case *ast.StarExpr:
		switch u := c.info.Types[lhs].Underlying().(type) {
		case *types.Struct, *types.Array:
			lVar := c.newVariable("l")
			rVar := c.newVariable("r")
			c.Printf("%s = %s;", lVar, c.translateExpr(l.X))
			c.Printf("%s = %s;", rVar, rhs)
			switch u2 := u.(type) {
			case *types.Struct:
				for i := 0; i < u2.NumFields(); i++ {
					field := u2.Field(i)
					c.Printf("%s.%s = %s.%s;", lVar, field.Name(), rVar, field.Name())
				}
			case *types.Array:
				iVar := c.newVariable("i")
				c.Printf("for (%s = 0; %s < %d; %s++) { %s[%s] = %s[%s]; }", iVar, iVar, u2.Len(), iVar, lVar, iVar, rVar, iVar)
			}
		default:
			c.Printf("%s.Go$set(%s);", c.translateExpr(l.X), rhs)
		}
	case *ast.IndexExpr:
		switch t := c.info.Types[l.X].Underlying().(type) {
		case *types.Array, *types.Pointer:
			c.Printf("%s[%s] = %s;", c.translateExpr(l.X), c.translateExpr(l.Index), rhs)
		case *types.Slice:
			c.Printf("Go$obj = %s;", c.translateExpr(l.X))
			c.Printf("Go$index = %s;", c.translateExpr(l.Index))
			c.Printf(`if (Go$index < 0 || Go$index >= Go$obj.length) { Go$throwRuntimeError("index out of range"); }`)
			c.Printf("Go$obj.array[Go$obj.offset + Go$index] = %s;", rhs)
		case *types.Map:
			keyVar := c.newVariable("_key")
			c.Printf("%s = %s;", keyVar, c.translateExprToType(l.Index, t.Key()))
			key := keyVar
			if hasId(t.Key()) {
				key = fmt.Sprintf("(%s || Go$Map.Go$nil).Go$key()", key)
			}
			c.Printf(`%s[%s] = { k: %s, v: %s };`, c.translateExpr(l.X), key, keyVar, rhs)
		default:
			panic(fmt.Sprintf("Unhandled lhs type: %T\n", t))
		}
	default:
		panic(fmt.Sprintf("Unhandled lhs type: %T\n", l))
	}
}

func hasFallthrough(caseClause *ast.CaseClause) bool {
	if len(caseClause.Body) == 0 {
		return false
	}
	b, isBranchStmt := caseClause.Body[len(caseClause.Body)-1].(*ast.BranchStmt)
	return isBranchStmt && b.Tok == token.FALLTHROUGH
}

type HasBreakVisitor struct {
	hasBreak bool
}

func (v *HasBreakVisitor) Visit(node ast.Node) (w ast.Visitor) {
	if v.hasBreak {
		return nil
	}
	switch n := node.(type) {
	case *ast.BranchStmt:
		if n.Tok == token.BREAK && n.Label == nil {
			v.hasBreak = true
			return nil
		}
	case *ast.FuncLit, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		return nil
	}
	return v
}